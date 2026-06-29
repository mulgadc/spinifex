package main

import (
	"context"
	"log/slog"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// reconcile re-adopts containers that survived an agent restart. It lists the
// runtime's containers and, for each task still running in this cluster,
// re-registers its IAM credentials, re-attaches the exit-wait, and refreshes its
// RUNNING state on the gateway. The returned set seeds pollAssignments so the
// gateway's re-delivered assignments for these tasks are acked but not re-run.
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

	// Group this cluster's running containers back into their tasks. The Assign is
	// reconstructed from labels — enough to re-register creds and tear down netns.
	type task struct {
		as         *bus.Assign
		containers []ctrruntime.Container
	}
	tasks := map[string]*task{}
	for _, c := range containers {
		if !c.Running || c.Labels[labelClusterName] != a.id.ClusterName {
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
		t.containers = append(t.containers, c)
	}

	for taskID, t := range tasks {
		a.registerTaskCreds(t.as)
		statuses := make([]bus.ContainerStatus, 0, len(t.containers))
		for _, c := range t.containers {
			name := c.Labels[labelContainerName]
			go a.waitContainer(ctx, t.as, name, c.ID)
			statuses = append(statuses, bus.ContainerStatus{Name: name, Status: bus.TaskStatusRunning, ContainerID: c.ID})
		}
		a.reportTaskState(t.as, bus.TaskStatusRunning, "", statuses)
		adopted[taskID] = true
		slog.Info("ecs-agent: re-adopted running task", "task", taskID, "containers", len(t.containers))
	}
	return adopted
}
