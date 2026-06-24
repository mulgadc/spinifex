package handlers_ecs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
)

// reservePlacementRetries bounds the CAS retry loop when concurrent RunTask calls
// contend for the same instance's capacity.
const reservePlacementRetries = 5

// RunTask places input.Count tasks of a definition onto container instances via
// bin-pack, reserves their capacity in KV, writes PENDING task records, and
// publishes an assign on the Layer-2 bus for each. Placement failures for a task
// are returned as RunTask failures; already-placed tasks in the same call stay.
func (s *Service) RunTask(input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	var clusterRec ClusterRecord
	found, err := getJSON(kv, ClusterMetaKey(cluster), &clusterRec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	taskDef, err := s.resolveTaskDef(kv, aws.StringValue(input.TaskDefinition))
	if err != nil {
		return nil, err
	}

	count := int(aws.Int64Value(input.Count))
	if count <= 0 {
		count = 1
	}
	strategy := placementStrategyFromAWS(input.PlacementStrategy)
	cpu, mem := taskDef.reservedCPU(), taskDef.reservedMemory()

	out := &ecs.RunTaskOutput{}
	for i := 0; i < count; i++ {
		taskID := uuid.NewString()
		inst, err := s.reservePlacement(kv, cluster, taskID, cpu, mem, strategy)
		if err != nil {
			out.Failures = append(out.Failures, &ecs.Failure{
				Reason: aws.String("RESOURCE:placement"),
				Detail: aws.String(err.Error()),
			})
			continue
		}

		rec := s.newTaskRecord(accountID, cluster, taskID, taskDef, inst, cpu, mem)
		if err := putJSON(kv, TaskKey(cluster, taskID), rec); err != nil {
			return nil, err
		}
		if err := s.publishAssign(accountID, cluster, inst.InstanceID, rec, taskDef); err != nil {
			slog.Error("ECS RunTask: failed to publish assign", "task", taskID, "instance", inst.InstanceID, "err", err)
		}
		out.Tasks = append(out.Tasks, s.taskToAWS(accountID, rec))
	}
	return out, nil
}

// reservePlacement bin-packs a task onto an ACTIVE instance and commits the
// reservation under a KV CAS, retrying on contention.
func (s *Service) reservePlacement(kv nats.KeyValue, cluster, taskID string, cpu, mem int, strategy string) (*InstanceRecord, error) {
	for range reservePlacementRetries {
		instances, err := s.listInstanceRecords(kv, cluster)
		if err != nil {
			return nil, err
		}
		chosen, err := placeTask(instances, cpu, mem, strategy)
		if err != nil {
			return nil, err
		}

		// Re-read the chosen instance for its current revision, then CAS-update.
		entry, err := kv.Get(InstanceKey(cluster, chosen.InstanceID))
		if err != nil {
			continue
		}
		var live InstanceRecord
		if uerr := json.Unmarshal(entry.Value(), &live); uerr != nil {
			return nil, uerr
		}
		if !live.fits(cpu, mem) {
			continue
		}
		live.ReservedCPU += cpu
		live.ReservedMemoryMiB += mem
		live.PlacedTasks = append(live.PlacedTasks, taskID)
		data, merr := json.Marshal(&live)
		if merr != nil {
			return nil, merr
		}
		if _, uerr := kv.Update(InstanceKey(cluster, live.InstanceID), data, entry.Revision()); uerr != nil {
			continue // lost the CAS race; retry placement
		}
		return &live, nil
	}
	return nil, fmt.Errorf("placement contended after %d attempts", reservePlacementRetries)
}

// newTaskRecord builds a PENDING task record for a placed task.
func (s *Service) newTaskRecord(accountID, cluster, taskID string, td *TaskDefRecord, inst *InstanceRecord, cpu, mem int) *TaskRecord {
	now := time.Now().UTC()
	rec := &TaskRecord{
		TaskID:               taskID,
		ARN:                  TaskARN(s.region, accountID, cluster, taskID),
		Cluster:              cluster,
		TaskDefFamily:        td.Family,
		TaskDefRevision:      td.Revision,
		TaskDefARN:           td.ARN,
		ContainerInstanceID:  inst.InstanceID,
		ContainerInstanceARN: inst.ARN,
		DesiredStatus:        TaskStatusRunning,
		LastStatus:           TaskStatusPending,
		ReservedCPU:          cpu,
		ReservedMemoryMiB:    mem,
		CreatedAt:            now,
	}
	for _, c := range td.Containers {
		rec.Containers = append(rec.Containers, ContainerState{Name: c.Name, Status: TaskStatusPending})
	}
	return rec
}

// publishAssign sends the task assignment to the instance's agent over the bus.
func (s *Service) publishAssign(accountID, cluster, instanceID string, rec *TaskRecord, td *TaskDefRecord) error {
	msg := bus.Assign{
		AccountID:       accountID,
		ClusterName:     cluster,
		InstanceID:      instanceID,
		TaskID:          rec.TaskID,
		TaskARN:         rec.ARN,
		TaskDefFamily:   td.Family,
		TaskDefRevision: td.Revision,
		AssignedAt:      time.Now().UTC(),
	}
	for _, c := range td.Containers {
		msg.Containers = append(msg.Containers, c.toAssignContainer())
	}
	data, err := json.Marshal(&msg)
	if err != nil {
		return err
	}
	subj := bus.AssignSubject(accountID, cluster, instanceID)
	return s.nc.Publish(subj, data)
}

// DescribeTasks returns task records for the named tasks in a cluster.
func (s *Service) DescribeTasks(input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeTasksOutput{}
	for _, ref := range awsStringSlice(input.Tasks) {
		taskID := containerInstanceShortID(ref)
		var rec TaskRecord
		found, err := getJSON(kv, TaskKey(cluster, taskID), &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.Tasks = append(out.Tasks, s.taskToAWS(accountID, &rec))
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// ListTasks returns the ARNs of all tasks in a cluster.
func (s *Service) ListTasks(input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	keys, err := keysWithPrefix(kv, TasksPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := &ecs.ListTasksOutput{}
	for _, k := range keys {
		var rec TaskRecord
		found, err := getJSON(kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.TaskArns = append(out.TaskArns, aws.String(rec.ARN))
		}
	}
	return out, nil
}

func (s *Service) taskToAWS(accountID string, r *TaskRecord) *ecs.Task {
	t := &ecs.Task{
		TaskArn:              aws.String(r.ARN),
		ClusterArn:           aws.String(ClusterARN(s.region, accountID, r.Cluster)),
		TaskDefinitionArn:    aws.String(r.TaskDefARN),
		DesiredStatus:        aws.String(r.DesiredStatus),
		LastStatus:           aws.String(r.LastStatus),
		ContainerInstanceArn: aws.String(r.ContainerInstanceARN),
	}
	if r.StoppedReason != "" {
		t.StoppedReason = aws.String(r.StoppedReason)
	}
	for _, c := range r.Containers {
		ctr := &ecs.Container{Name: aws.String(c.Name), LastStatus: aws.String(c.Status)}
		if c.ExitCode != nil {
			ctr.ExitCode = aws.Int64(int64(*c.ExitCode))
		}
		t.Containers = append(t.Containers, ctr)
	}
	return t
}

// placementStrategyFromAWS extracts the first strategy type, defaulting binpack.
func placementStrategyFromAWS(in []*ecs.PlacementStrategy) string {
	for _, p := range in {
		if p != nil {
			return aws.StringValue(p.Type)
		}
	}
	return StrategyBinpack
}
