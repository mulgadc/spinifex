package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// subscribeAssign wires the per-instance assign subject to the task runner. It is
// a no-op when the agent has no live NATS connection (unit tests drive runTask
// directly).
func (a *Agent) subscribeAssign(ctx context.Context) error {
	if a.nc == nil {
		return nil
	}
	subj := bus.AssignSubject(a.id.AccountID, a.id.ClusterName, a.id.InstanceID)
	sub, err := a.nc.Subscribe(subj, func(msg *nats.Msg) {
		var as bus.Assign
		if err := json.Unmarshal(msg.Data, &as); err != nil {
			slog.Error("ecs-agent: bad assign payload", "err", err)
			return
		}
		slog.Info("ecs-agent: task assigned", "task", as.TaskID, "containers", len(as.Containers))
		go a.runTask(ctx, &as)
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", subj, err)
	}
	a.closers = append(a.closers, sub.Unsubscribe)
	slog.Info("ecs-agent: assign subscription active", "subject", subj)
	return nil
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

// reportTaskState publishes a TaskState message on the task's state subject.
func (a *Agent) reportTaskState(as *bus.Assign, status, reason string, containers []bus.ContainerStatus) {
	if a.pub == nil {
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
	data, err := json.Marshal(&msg)
	if err != nil {
		slog.Error("ecs-agent: marshal task-state failed", "task", as.TaskID, "err", err)
		return
	}
	subj := bus.TaskStateSubject(a.id.AccountID, a.id.ClusterName, as.TaskID)
	if err := a.pub.Publish(subj, data); err != nil {
		slog.Error("ecs-agent: publish task-state failed", "task", as.TaskID, "subject", subj, "err", err)
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
