package handlers_ecs

import "context"

// ReportTaskGPUInput is the agent's per-task GPU device-assignment report
// (agent → gateway). Internal agent↔gateway shape, not an AWS SDK action:
// real AWS's SubmitTaskStateChange carries no gpuIds field, so the agent's
// local ledger assignment is reported out-of-band and merged onto the task
// record here, ahead of DescribeTasks.
type ReportTaskGPUInput struct {
	Cluster    string               `json:"cluster"`
	Task       string               `json:"task"`
	Containers []ContainerGPUReport `json:"containers,omitempty"`
}

// ContainerGPUReport is one container's pinned device UUIDs.
type ContainerGPUReport struct {
	Name   string   `json:"name"`
	GPUIDs []string `json:"gpuIds,omitempty"`
}

// ReportTaskGPUOutput acknowledges the report.
type ReportTaskGPUOutput struct {
	Acknowledgment string `json:"acknowledgment"`
}

// ReportTaskGPU merges the agent-reported device UUIDs onto the task's
// container records by name, so DescribeTasks can surface gpuIds. A missing
// task or container is a silent no-op — the state-report path already owns
// the task's lifecycle; this only enriches it.
func (s *Service) ReportTaskGPU(ctx context.Context, input *ReportTaskGPUInput, accountID string) (*ReportTaskGPUOutput, error) {
	cluster := clusterShortName(input.Cluster)
	taskID := taskShortID(input.Task)
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var task TaskRecord
	found, err := getJSON(ctx, kv, TaskKey(cluster, taskID), &task)
	if err != nil {
		return nil, err
	}
	if !found {
		return &ReportTaskGPUOutput{Acknowledgment: "OK"}, nil
	}
	byName := make(map[string][]string, len(input.Containers))
	for _, c := range input.Containers {
		byName[c.Name] = c.GPUIDs
	}
	changed := false
	for i := range task.Containers {
		if ids, ok := byName[task.Containers[i].Name]; ok {
			task.Containers[i].GPUIDs = ids
			changed = true
		}
	}
	if changed {
		if err := putJSON(ctx, kv, TaskKey(cluster, taskID), &task); err != nil {
			return nil, err
		}
	}
	return &ReportTaskGPUOutput{Acknowledgment: "OK"}, nil
}
