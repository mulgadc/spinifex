package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

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

	var ackNext []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			assigns, err := a.cp.PollAssignments(a.id.ClusterName, a.id.InstanceID, ackNext)
			if err != nil {
				slog.Warn("ecs-agent: poll assignments failed", "err", err)
				continue
			}
			ackNext = ackNext[:0]
			for i := range assigns {
				as := assigns[i]
				if !dispatched[as.TaskID] {
					dispatched[as.TaskID] = true
					slog.Info("ecs-agent: task assigned", "task", as.TaskID, "containers", len(as.Containers))
					go a.runTask(ctx, &as)
				}
				ackNext = append(ackNext, as.TaskID)
			}
		}
	}
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
		if _, err := a.puller.Pull(ctx, ctrruntime.PullSpec{Ref: c.Image}, resolver); err != nil {
			slog.Error("ecs-agent: pull failed", "task", as.TaskID, "image", c.Image, "err", err)
			a.teardownTaskNetns(as)
			a.reportTaskState(as, bus.TaskStatusStopped, "image pull failed: "+err.Error(), statuses)
			return
		}

		cid := containerID(as.TaskID, c.Name)
		spec := ctrruntime.RunSpec{
			Image:     c.Image,
			Command:   c.Command,
			Env:       withCredEnv(c.Environment, credID),
			Labels:    taskLabels(as, c.Name),
			NetnsPath: netnsPath,
		}
		id, err := a.runner.Run(ctx, cid, spec)
		if err != nil {
			slog.Error("ecs-agent: run failed", "task", as.TaskID, "container", c.Name, "err", err)
			a.teardownTaskNetns(as)
			a.reportTaskState(as, bus.TaskStatusStopped, "container start failed: "+err.Error(), statuses)
			return
		}
		statuses = append(statuses, bus.ContainerStatus{Name: c.Name, Status: bus.TaskStatusRunning, ContainerID: id})
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

// reportTaskState reports a task transition through the gateway's
// SubmitTaskStateChange action.
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
