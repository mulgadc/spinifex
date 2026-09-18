package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// defaultStopTimeout is applied when a container carries no explicit
// stopTimeout, matching the EC2 launch-type default ECS itself uses.
const defaultStopTimeout = 30 * time.Second

// pollAssignments drains the instance's assignment inbox on a ticker until ctx is
// cancelled. Each assign is dispatched to runTask exactly once; the taskIDs seen
// on a poll are acked on the next poll so the gateway can drop them — a crash
// before ack re-delivers (at-least-once), matching ACS. A failed poll is logged
// and retried on the next tick rather than killing the loop. dispatched is seeded
// by the startup reconcile with tasks already adopted from running containers, so
// their re-delivered assignments are acked but not re-run.
func (a *Agent) pollAssignments(ctx context.Context, dispatched map[string]bool) {
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	reaps := newStopLedger()
	var ackAssigns, ackStops []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			assigns, stops, err := a.cp.PollAssignments(a.id.ClusterName, a.id.InstanceID, ackAssigns, ackStops)
			if err != nil {
				slog.Warn("ecs-agent: poll assignments failed", "err", err)
				continue
			}
			// Dispatch stops first so a task stopped in the same poll it was assigned
			// is never started.
			ackStops = ackStops[:0]
			for i := range stops {
				sd := stops[i]
				switch {
				case reaps.done(sd.TaskID):
					ackStops = append(ackStops, sd.TaskID)
				case reaps.begin(sd.TaskID):
					slog.Info("ecs-agent: task stop requested", "task", sd.TaskID, "reason", sd.Reason)
					go func() { reaps.end(sd.TaskID, a.stopTask(ctx, sd)) }()
				}
			}
			ackAssigns = ackAssigns[:0]
			for i := range assigns {
				as := assigns[i]
				if !dispatched[as.TaskID] && !reaps.seen(as.TaskID) {
					dispatched[as.TaskID] = true
					slog.Info("ecs-agent: task assigned", "task", as.TaskID, "containers", len(as.Containers))
					go a.runTask(ctx, &as)
				}
				ackAssigns = append(ackAssigns, as.TaskID)
			}
		}
	}
}

// stopLedger tracks reaps in flight and reaps that reported STOPPED. The two
// are separate because only the second may be acked: acking the first deletes a
// directive that was never performed, and nothing re-posts it.
type stopLedger struct {
	mu       sync.Mutex
	inflight map[string]bool
	stopped  map[string]bool
}

func newStopLedger() *stopLedger {
	return &stopLedger{inflight: map[string]bool{}, stopped: map[string]bool{}}
}

// begin claims a task for a reap, reporting false when one is already running.
func (l *stopLedger) begin(taskID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[taskID] {
		return false
	}
	l.inflight[taskID] = true
	return true
}

// end releases the claim, recording the task as stopped only if the reap got
// that far. A failed reap leaves nothing behind, so the next poll redelivers
// the directive and dispatches it again.
func (l *stopLedger) end(taskID string, reported bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.inflight, taskID)
	if reported {
		l.stopped[taskID] = true
	}
}

// done reports whether the task's reap reported STOPPED, which is the only
// condition under which its directive may be acked.
func (l *stopLedger) done(taskID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped[taskID]
}

// seen reports whether a stop for the task is in flight or complete, so an
// assignment arriving alongside its own stop is never started.
func (l *stopLedger) seen(taskID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[taskID] || l.stopped[taskID]
}

// stopTask reaps a task's containers on a control-plane stop directive: it
// lists the runtime's containers, gracefully stops (SIGTERM, wait up to the
// container's stopTimeout, then force kill + delete) the ones labeled with
// this task, then reports the task STOPPED with the directive's reason.
// Idempotent — a task with no live containers still reports STOPPED so the
// scheduler releases its capacity. Reports whether it got as far as saying
// STOPPED: a caller may only ack the directive when it did.
func (a *Agent) stopTask(ctx context.Context, sd bus.StopDirective) bool {
	if a.runner == nil {
		slog.Warn("ecs-agent: stop deferred, no container runtime", "task", sd.TaskID)
		return false
	}
	containers, err := a.runner.List(ctx)
	if err != nil {
		slog.Warn("ecs-agent: stop list failed", "task", sd.TaskID, "err", err)
		return false
	}
	statuses := make([]bus.ContainerStatus, 0)
	for _, c := range containers {
		if c.Labels[labelTaskID] != sd.TaskID {
			continue
		}
		if rerr := a.runner.Stop(ctx, c.ID, containerStopTimeout(c.Labels)); rerr != nil {
			slog.Warn("ecs-agent: stop failed", "task", sd.TaskID, "container", c.ID, "err", rerr)
		}
		statuses = append(statuses, bus.ContainerStatus{
			Name: c.Labels[labelContainerName], Status: bus.TaskStatusStopped, ContainerID: c.ID,
		})
	}
	reason := sd.Reason
	if reason == "" {
		reason = "Task stopped"
	}
	a.reportTaskState(&bus.Assign{TaskID: sd.TaskID}, bus.TaskStatusStopped, reason, statuses)
	slog.Info("ecs-agent: task reaped", "task", sd.TaskID, "containers", len(statuses))
	return true
}

// runTask pulls each container image, starts the containers, and reports task
// state on the bus. A missing runtime or any per-container failure reports the
// task STOPPED with a reason; success reports RUNNING and waits for exit.
func (a *Agent) runTask(ctx context.Context, as *bus.Assign) {
	if a.puller == nil || a.runner == nil {
		a.reportTaskState(as, bus.TaskStatusStopped, "containerd unavailable on agent", nil)
		return
	}

	// awsvpc: build the task netns from the hot-plugged ENI before any container.
	netnsPath, err := a.setupTaskNetns(as)
	if err != nil {
		slog.Error("ecs-agent: task netns setup failed", "task", as.TaskID, "err", err)
		a.reportTaskState(as, bus.TaskStatusStopped, "network setup failed: "+err.Error(), nil)
		return
	}

	credID := a.registerTaskCreds(as)
	resolver := a.pullResolver(as)

	statuses := make([]bus.ContainerStatus, 0, len(as.Containers))
	for _, c := range as.Containers {
		// startTimeout bounds pull-through-start as a unit: real ECS measures a
		// container's time-to-RUNNING from the same starting point.
		runCtx, cancel := containerStartCtx(ctx, c.StartTimeout)

		if _, err := a.puller.Pull(runCtx, ctrruntime.PullSpec{Ref: c.Image}, resolver); err != nil {
			cancel()
			slog.Error("ecs-agent: pull failed", "task", as.TaskID, "image", c.Image, "err", err)
			a.teardownTaskNetns(as)
			a.reportTaskState(as, bus.TaskStatusStopped, "image pull failed: "+err.Error(), statuses)
			return
		}

		cid := containerID(as.TaskID, c.Name)
		gpuIDs := a.pinContainerGPUs(as.TaskID, c.Name, c.GPU)
		labels := taskLabels(as, c.Name)
		if c.StopTimeout != nil {
			labels[labelStopTimeout] = strconv.FormatInt(*c.StopTimeout, 10)
		}
		spec := ctrruntime.RunSpec{
			Image:                  c.Image,
			Command:                c.Command,
			Env:                    withCredEnv(c.Environment, credID),
			Labels:                 labels,
			NetnsPath:              netnsPath,
			GPU:                    c.GPU,
			GPUIDs:                 gpuIDs,
			User:                   c.User,
			ReadonlyRootFilesystem: c.ReadonlyRootFilesystem,
			Privileged:             c.Privileged,
			PseudoTerminal:         c.PseudoTerminal,
			Interactive:            c.Interactive,
			SystemControls:         toRuntimeSystemControls(c.SystemControls),
			CapAdd:                 c.CapAdd,
			CapDrop:                c.CapDrop,
		}
		id, err := a.runner.Run(runCtx, cid, spec)
		cancel()
		if err != nil {
			slog.Error("ecs-agent: run failed", "task", as.TaskID, "container", c.Name, "err", err)
			a.teardownTaskNetns(as)
			a.reportTaskState(as, bus.TaskStatusStopped, "container start failed: "+err.Error(), statuses)
			return
		}
		statuses = append(statuses, bus.ContainerStatus{
			Name: c.Name, Status: bus.TaskStatusRunning, ContainerID: id, GPUIDs: gpuIDs,
		})
		go a.waitContainer(ctx, as, c.Name, id)
	}

	a.reportTaskState(as, bus.TaskStatusRunning, "", statuses)
	slog.Info("ecs-agent: task running", "task", as.TaskID, "containers", len(statuses))
}

// waitContainer blocks until a container exits, then reports the task STOPPED
// with the exit code. v1 stops the whole task when its first container exits.
func (a *Agent) waitContainer(ctx context.Context, as *bus.Assign, name, containerID string) {
	status, err := a.runner.Wait(ctx, containerID)
	if err != nil {
		slog.Warn("ecs-agent: wait container failed", "task", as.TaskID, "container", name, "err", err)
		return
	}
	exit := status.ExitCode
	a.teardownTaskNetns(as)
	statuses := []bus.ContainerStatus{{Name: name, Status: bus.TaskStatusStopped, ContainerID: containerID, ExitCode: &exit}}
	a.reportTaskState(as, bus.TaskStatusStopped, fmt.Sprintf("container %s exited (%d)", name, exit), statuses)
	slog.Info("ecs-agent: task stopped", "task", as.TaskID, "container", name, "exitCode", exit)
}

// setupTaskNetns builds the awsvpc task netns from the hot-plugged ENI. Bridge/
// host tasks (no ENI MAC) and a nil controller are no-ops returning an empty path.
func (a *Agent) setupTaskNetns(as *bus.Assign) (string, error) {
	if as.ENIMacAddress == "" || a.netns == nil {
		return "", nil
	}
	return a.netns.Setup(as.TaskID, as.ENIMacAddress)
}

// teardownTaskNetns releases the task's IAM credential registration, then removes
// the awsvpc task netns (the latter is a no-op for bridge/host tasks). It runs at
// every task-stop path so credentials never outlive the task that owns them.
func (a *Agent) teardownTaskNetns(as *bus.Assign) {
	if a.cred != nil {
		a.cred.Deregister(taskCredID(as))
	}
	if as.ENIMacAddress == "" || a.netns == nil {
		return
	}
	if err := a.netns.Teardown(as.TaskID); err != nil {
		slog.Warn("ecs-agent: task netns teardown", "task", as.TaskID, "err", err)
	}
}

// pullResolver returns the ECR resolver used for a task's image pulls. When the
// assign carries an execution role, it authorizes pulls as that role (assumed
// over the gateway); an empty role or a nil credential endpoint (unit tests)
// falls back to the instance-role resolver, so a pull is never worse off.
func (a *Agent) pullResolver(as *bus.Assign) ctrruntime.Resolver {
	if as.ExecutionRoleARN == "" || a.cred == nil {
		return a.resolver
	}
	prov := a.cred.AssumeProvider(as.ExecutionRoleARN, sessionName(as.TaskID))
	return newLazyECRResolver(prov, a.cfg.Region, a.cfg.GatewayURL, a.cfg.GatewayCA)
}

// registerTaskCreds registers the task's credID -> role mapping with the
// credential endpoint and returns the credID (empty when the task has no role).
func (a *Agent) registerTaskCreds(as *bus.Assign) string {
	credID := taskCredID(as)
	if credID == "" || a.cred == nil {
		return credID
	}
	a.cred.Register(credID, as.TaskRoleARN)
	return credID
}

// taskCredID is the credential ID a task's containers fetch credentials under:
// the scheduler-assigned CredID, falling back to the taskID. Empty when the task
// carries no IAM role.
func taskCredID(as *bus.Assign) string {
	if as.TaskRoleARN == "" {
		return ""
	}
	if as.CredID != "" {
		return as.CredID
	}
	return as.TaskID
}

// withCredEnv returns env with AWS_CONTAINER_CREDENTIALS_RELATIVE_URI set for
// credID, copying the map so the assign's container env is left untouched. A
// blank credID (no task role) returns env unchanged.
func withCredEnv(env map[string]string, credID string) map[string]string {
	if credID == "" {
		return env
	}
	out := make(map[string]string, len(env)+1)
	maps.Copy(out, env)
	out["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] = credRelativeURI(credID)
	return out
}

// containerStartCtx bounds pull+run by a container's startTimeout, if set. A
// nil startTimeout (the common case) returns ctx unchanged with a no-op
// cancel, matching today's unbounded behaviour.
func containerStartCtx(ctx context.Context, startTimeout *int64) (context.Context, context.CancelFunc) {
	if startTimeout == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(*startTimeout)*time.Second)
}

// containerStopTimeout reads the stopTimeout stamped on a container's labels
// by runTask, falling back to defaultStopTimeout when absent or unparseable.
func containerStopTimeout(labels map[string]string) time.Duration {
	v, ok := labels[labelStopTimeout]
	if !ok {
		return defaultStopTimeout
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return defaultStopTimeout
	}
	return time.Duration(secs) * time.Second
}

// toRuntimeSystemControls maps the assign's sysctl pairs to the runtime
// package's own type, keeping cmd/ecs-agent/runtime decoupled from the ecs
// handlers' bus package.
func toRuntimeSystemControls(in []bus.SystemControl) []ctrruntime.SystemControl {
	if len(in) == 0 {
		return nil
	}
	out := make([]ctrruntime.SystemControl, len(in))
	for i, sc := range in {
		out[i] = ctrruntime.SystemControl{Namespace: sc.Namespace, Value: sc.Value}
	}
	return out
}

// pinContainerGPUs reserves n device UUIDs for a container from the local
// ledger, before the container is started so the runner can inject them as
// CDI devices. A short ledger (fewer free devices than requested, e.g.
// discovery found none) is logged and treated as best-effort: the container
// still runs without pinned UUIDs (and without GPU access) rather than
// failing the task.
func (a *Agent) pinContainerGPUs(taskID, containerName string, n int) []string {
	if n <= 0 || a.gpu == nil {
		return nil
	}
	uuids, err := a.gpu.Pin(gpuKey(taskID, containerName), n)
	if err != nil {
		slog.Warn("ecs-agent: gpu pin short", "task", taskID, "container", containerName, "err", err)
	}
	return uuids
}

// reportTaskState reports a task transition through the gateway's
// SubmitTaskStateChange action, then (RUNNING with pinned GPUs) reports the
// per-container device UUIDs, and (STOPPED) releases them back to the ledger.
func (a *Agent) reportTaskState(as *bus.Assign, status, reason string, containers []bus.ContainerStatus) {
	if a.cp == nil {
		return
	}
	msg := bus.TaskState{
		AccountID:   a.id.AccountID,
		ClusterName: a.id.ClusterName,
		InstanceID:  a.id.InstanceID,
		TaskID:      as.TaskID,
		LastStatus:  status,
		Containers:  containers,
		Reason:      reason,
		ReportedAt:  time.Now().UTC(),
	}
	if err := a.cp.SubmitTaskState(msg); err != nil {
		slog.Error("ecs-agent: report task-state failed", "task", as.TaskID, "err", err)
	}
	a.reportTaskGPUs(as.TaskID, containers)
	if status == bus.TaskStatusStopped && a.gpu != nil {
		a.gpu.ReleaseTask(as.TaskID)
	}
}

// reportTaskGPUs sends the control plane the pinned device UUIDs for any
// container in the report that has them, so DescribeTasks can surface gpuIds.
// A no-op when nothing was pinned (the common non-GPU-task case).
func (a *Agent) reportTaskGPUs(taskID string, containers []bus.ContainerStatus) {
	reports := make([]handlers_ecs.ContainerGPUReport, 0, len(containers))
	for _, c := range containers {
		if len(c.GPUIDs) > 0 {
			reports = append(reports, handlers_ecs.ContainerGPUReport{Name: c.Name, GPUIDs: c.GPUIDs})
		}
	}
	if len(reports) == 0 {
		return
	}
	if err := a.cp.ReportTaskGPU(a.id.ClusterName, taskID, reports); err != nil {
		slog.Error("ecs-agent: report task-gpu failed", "task", taskID, "err", err)
	}
}

// containerID composes a containerd-valid container ID from a task + container.
func containerID(taskID, name string) string {
	return fmt.Sprintf("%s-%s", taskID, name)
}

// mulga.ecs.* label keys stamped on every container. The reboot reconciler reads
// them back to re-associate running containers with their task on restart; the
// cred/role/MAC labels make a container self-describing enough to re-register its
// credentials and tear down its netns without the original assignment.
const (
	labelTaskID        = "mulga.ecs.taskID"
	labelContainerName = "mulga.ecs.containerName"
	labelClusterName   = "mulga.ecs.clusterName"
	labelCredID        = "mulga.ecs.credID"
	labelTaskRoleARN   = "mulga.ecs.taskRoleArn"
	labelENIMac        = "mulga.ecs.eniMac"
	// labelStopTimeout carries a container's stopTimeout (seconds) so stopTask
	// can read it back from List's labels; it has no assign to consult there.
	labelStopTimeout = "mulga.ecs.stopTimeout"
)

// taskLabels are the mulga.ecs.* labels stamped on a container. The cred/role/MAC
// labels are omitted when empty so a container carries only what it needs.
func taskLabels(as *bus.Assign, name string) map[string]string {
	labels := map[string]string{
		labelTaskID:        as.TaskID,
		labelContainerName: name,
		labelClusterName:   as.ClusterName,
	}
	if credID := taskCredID(as); credID != "" {
		labels[labelCredID] = credID
	}
	if as.TaskRoleARN != "" {
		labels[labelTaskRoleARN] = as.TaskRoleARN
	}
	if as.ENIMacAddress != "" {
		labels[labelENIMac] = as.ENIMacAddress
	}
	return labels
}
