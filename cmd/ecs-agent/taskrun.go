package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// pollAssignments drains the instance's assignment inbox on a ticker until ctx is
// cancelled. Each assign is dispatched to runTask exactly once; the taskIDs seen
// on a poll are acked on the next poll so the gateway can drop them — a crash
// before ack re-delivers (at-least-once), matching ACS. A failed poll is logged
// and retried on the next tick rather than killing the loop.
func (a *Agent) pollAssignments(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	dispatched := map[string]bool{}
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

	statuses := make([]bus.ContainerStatus, 0, len(as.Containers))
	for _, c := range as.Containers {
		if _, err := a.puller.Pull(ctx, ctrruntime.PullSpec{Ref: c.Image}, a.resolver); err != nil {
			slog.Error("ecs-agent: pull failed", "task", as.TaskID, "image", c.Image, "err", err)
			a.reportTaskState(as, bus.TaskStatusStopped, "image pull failed: "+err.Error(), statuses)
			return
		}

		cid := containerID(as.TaskID, c.Name)
		spec := ctrruntime.RunSpec{
			Image:   c.Image,
			Command: c.Command,
			Env:     c.Environment,
			Labels:  taskLabels(as, c.Name),
		}
		id, err := a.runner.Run(ctx, cid, spec)
		if err != nil {
			slog.Error("ecs-agent: run failed", "task", as.TaskID, "container", c.Name, "err", err)
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
	statuses := []bus.ContainerStatus{{Name: name, Status: bus.TaskStatusStopped, ContainerID: containerID, ExitCode: &exit}}
	a.reportTaskState(as, bus.TaskStatusStopped, fmt.Sprintf("container %s exited (%d)", name, exit), statuses)
	slog.Info("ecs-agent: task stopped", "task", as.TaskID, "container", name, "exitCode", exit)
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

// taskLabels are the mulga.ecs.* labels stamped on every container so the reboot
// reconciler can re-associate running containers with their task on restart.
func taskLabels(as *bus.Assign, name string) map[string]string {
	return map[string]string{
		"mulga.ecs.taskID":        as.TaskID,
		"mulga.ecs.containerName": name,
		"mulga.ecs.clusterName":   as.ClusterName,
	}
}
