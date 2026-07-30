package main

import (
	"context"
	"log/slog"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// reconcile re-syncs the gateway with what this instance is actually running
// after an agent restart. It lists the runtime's containers and, for each task
// in this cluster, either re-adopts it (still running: re-register IAM
// credentials, re-attach the exit-wait, refresh RUNNING) or reports it STOPPED
// and clears its leftovers. The returned set seeds pollAssignments so the
// gateway's re-delivered assignments for these tasks are acked but not re-run.
//
// Reporting the dead ones is the load-bearing half. The exit-wait is cancelled
// when the agent shuts down, so a container that dies while the agent is down
// has nobody to report its exit; the gateway holds the task RUNNING forever,
// its service never replaces it, and the target never recovers.
//
// It is best-effort: a nil runner or a List error returns an empty set and the
// agent falls back to its pre-reconcile behaviour (poll + run).
func (a *Agent) reconcile(ctx context.Context) map[string]bool {
	adopted := map[string]bool{}
	if a.runner == nil {
		return adopted
	}
	containers, err := a.runner.List(ctx)
	if err != nil {
		slog.Warn("ecs-agent: reconcile list failed", "err", err)
		return adopted
	}

	// Group this cluster's containers back into their tasks, running or not. The
	// Assign is reconstructed from labels — enough to re-register creds and tear
	// down netns.
	type task struct {
		as      *bus.Assign
		running []ctrruntime.Container
		dead    []ctrruntime.Container
	}
	tasks := map[string]*task{}
	for _, c := range containers {
		if c.Labels[labelClusterName] != a.id.ClusterName {
			continue
		}
		taskID := c.Labels[labelTaskID]
		if taskID == "" {
			continue
		}
		t := tasks[taskID]
		if t == nil {
			t = &task{as: &bus.Assign{
				TaskID:        taskID,
				ClusterName:   a.id.ClusterName,
				CredID:        c.Labels[labelCredID],
				TaskRoleARN:   c.Labels[labelTaskRoleARN],
				ENIMacAddress: c.Labels[labelENIMac],
			}}
			tasks[taskID] = t
		}
		if c.Running {
			t.running = append(t.running, c)
		} else {
			t.dead = append(t.dead, c)
		}
	}

	for taskID, t := range tasks {
		// A task with nothing running is over, whatever the gateway still believes.
		// Mixed running/dead is left to the running path: the exit-wait re-attached
		// below reports the individual container when it is reaped.
		if len(t.running) == 0 {
			a.reportStoppedTask(ctx, t.as, t.dead)
			adopted[taskID] = true
			continue
		}

		a.registerTaskCreds(t.as)
		statuses := make([]bus.ContainerStatus, 0, len(t.running))
		for _, c := range t.running {
			name := c.Labels[labelContainerName]
			go a.waitContainer(ctx, t.as, name, c.ID)
			statuses = append(statuses, bus.ContainerStatus{Name: name, Status: bus.TaskStatusRunning, ContainerID: c.ID})
		}
		a.reportTaskState(t.as, bus.TaskStatusRunning, "", statuses)
		adopted[taskID] = true
		slog.Info("ecs-agent: re-adopted running task", "task", taskID, "containers", len(t.running))
	}
	return adopted
}

// reportStoppedTask clears a task's leftover containers and tells the gateway it
// is STOPPED, so the scheduler stops counting a task this instance is not
// running and its service can place a replacement.
func (a *Agent) reportStoppedTask(ctx context.Context, as *bus.Assign, dead []ctrruntime.Container) {
	statuses := make([]bus.ContainerStatus, 0, len(dead))
	for _, c := range dead {
		if err := a.runner.Remove(ctx, c.ID); err != nil {
			slog.Warn("ecs-agent: reconcile remove failed", "task", as.TaskID, "container", c.ID, "err", err)
		}
		statuses = append(statuses, bus.ContainerStatus{
			Name: c.Labels[labelContainerName], Status: bus.TaskStatusStopped, ContainerID: c.ID,
		})
	}
	a.reportTaskState(as, bus.TaskStatusStopped, "Container exited while the agent was not running", statuses)
	slog.Info("ecs-agent: reported stopped task", "task", as.TaskID, "containers", len(dead))
}
