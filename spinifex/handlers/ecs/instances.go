package handlers_ecs

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
)

// listInstanceRecords returns all container-instance records in a cluster.
func (s *Service) listInstanceRecords(kv nats.KeyValue, cluster string) ([]InstanceRecord, error) {
	keys, err := keysWithPrefix(kv, InstancesPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]InstanceRecord, 0, len(keys))
	for _, k := range keys {
		var rec InstanceRecord
		found, err := getJSON(kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
}

// RegisterContainerInstance is the AWS-API registration path. In 4e the agent
// normally registers over the Layer-2 bus; this keeps API parity by writing the
// same record shape from an explicit call.
func (s *Service) RegisterContainerInstance(input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	instanceID := aws.StringValue(input.InstanceIdentityDocument)
	if instanceID == "" {
		instanceID = "ci-" + time.Now().UTC().Format("20060102150405")
	}
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	rec, err := s.upsertInstance(kv, accountID, cluster, instanceID, func(r *InstanceRecord) {
		for _, res := range input.TotalResources {
			switch aws.StringValue(res.Name) {
			case "CPU":
				r.TotalCPU = int(aws.Int64Value(res.IntegerValue))
			case "MEMORY":
				r.TotalMemoryMiB = int(aws.Int64Value(res.IntegerValue))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return &ecs.RegisterContainerInstanceOutput{ContainerInstance: s.instanceToAWS(rec)}, nil
}

// DescribeContainerInstances returns records for the named container instances.
func (s *Service) DescribeContainerInstances(input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeContainerInstancesOutput{}
	for _, ref := range awsStringSlice(input.ContainerInstances) {
		id := containerInstanceShortID(ref)
		var rec InstanceRecord
		found, err := getJSON(kv, InstanceKey(cluster, id), &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.ContainerInstances = append(out.ContainerInstances, s.instanceToAWS(&rec))
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// ListContainerInstances returns the ARNs of all container instances in a cluster.
func (s *Service) ListContainerInstances(input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	recs, err := s.listInstanceRecords(kv, cluster)
	if err != nil {
		return nil, err
	}
	out := &ecs.ListContainerInstancesOutput{}
	for i := range recs {
		out.ContainerInstanceArns = append(out.ContainerInstanceArns, aws.String(recs[i].ARN))
	}
	return out, nil
}

// upsertInstance reads-or-creates the instance record, applies mutate, and writes
// it back. Used by both the AWS register path and the bus register handler.
func (s *Service) upsertInstance(kv nats.KeyValue, accountID, cluster, instanceID string, mutate func(*InstanceRecord)) (*InstanceRecord, error) {
	var rec InstanceRecord
	found, err := getJSON(kv, InstanceKey(cluster, instanceID), &rec)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !found {
		rec = InstanceRecord{
			InstanceID:   instanceID,
			ARN:          ContainerInstanceARN(s.region, accountID, cluster, instanceID),
			Cluster:      cluster,
			Status:       InstanceStatusActive,
			RegisteredAt: now,
		}
	}
	rec.LastSeen = now
	if mutate != nil {
		mutate(&rec)
	}
	if err := putJSON(kv, InstanceKey(cluster, instanceID), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Service) instanceToAWS(r *InstanceRecord) *ecs.ContainerInstance {
	registered := []*ecs.Resource{
		{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalCPU))},
		{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalMemoryMiB))},
	}
	remaining := []*ecs.Resource{
		{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalCPU - r.ReservedCPU))},
		{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalMemoryMiB - r.ReservedMemoryMiB))},
	}
	return &ecs.ContainerInstance{
		ContainerInstanceArn: aws.String(r.ARN),
		Ec2InstanceId:        aws.String(r.InstanceID),
		Status:               aws.String(r.Status),
		AgentConnected:       aws.Bool(r.Status == InstanceStatusActive),
		RunningTasksCount:    aws.Int64(int64(len(r.PlacedTasks))),
		RegisteredResources:  registered,
		RemainingResources:   remaining,
		VersionInfo:          &ecs.VersionInfo{AgentVersion: aws.String(r.AgentVersion)},
	}
}

// --- Layer-2 bus event handlers (called by the scheduler) ---

// recordRegister upserts a container-instance record from a bus RegisterInstance.
func (s *Service) recordRegister(msg *bus.RegisterInstance) error {
	kv, err := s.bucket(msg.AccountID)
	if err != nil {
		return err
	}
	_, err = s.upsertInstance(kv, msg.AccountID, msg.ClusterName, msg.InstanceID, func(r *InstanceRecord) {
		r.AZ = msg.AZ
		r.Hostname = msg.Hostname
		r.AgentVersion = msg.AgentVersion
		r.TotalCPU = msg.Capacity.CPU
		r.TotalMemoryMiB = msg.Capacity.MemoryMiB
		r.Status = InstanceStatusActive
	})
	return err
}

// recordHeartbeat refreshes an instance's LastSeen and status from a bus beat.
func (s *Service) recordHeartbeat(msg *bus.Heartbeat) error {
	kv, err := s.bucket(msg.AccountID)
	if err != nil {
		return err
	}
	var rec InstanceRecord
	found, err := getJSON(kv, InstanceKey(msg.ClusterName, msg.InstanceID), &rec)
	if err != nil || !found {
		return err
	}
	rec.LastSeen = time.Now().UTC()
	if msg.Status != "" {
		rec.Status = msg.Status
	}
	return putJSON(kv, InstanceKey(msg.ClusterName, msg.InstanceID), &rec)
}

// recordTaskState applies an agent task-state report: it updates the task record
// and, on STOPPED, releases the reserved capacity back to the instance.
func (s *Service) recordTaskState(msg *bus.TaskState) error {
	kv, err := s.bucket(msg.AccountID)
	if err != nil {
		return err
	}
	var task TaskRecord
	found, err := getJSON(kv, TaskKey(msg.ClusterName, msg.TaskID), &task)
	if err != nil || !found {
		return err
	}

	prev := task.LastStatus
	task.LastStatus = msg.LastStatus
	if len(msg.Containers) > 0 {
		task.Containers = task.Containers[:0]
		for _, c := range msg.Containers {
			task.Containers = append(task.Containers, ContainerState{
				Name: c.Name, Status: c.Status, ContainerID: c.ContainerID, ExitCode: c.ExitCode,
			})
		}
	}
	now := time.Now().UTC()
	if msg.LastStatus == TaskStatusRunning && task.StartedAt.IsZero() {
		task.StartedAt = now
	}
	if msg.LastStatus == TaskStatusStopped {
		task.StoppedAt = now
		if msg.Reason != "" {
			task.StoppedReason = msg.Reason
		}
	}
	if err := putJSON(kv, TaskKey(msg.ClusterName, msg.TaskID), &task); err != nil {
		return err
	}

	// Release capacity + reclaim the task ENI once, on the transition into STOPPED.
	if msg.LastStatus == TaskStatusStopped && prev != TaskStatusStopped {
		s.reclaimTaskENI(msg.AccountID, &task)
		return s.releaseReservation(kv, msg.ClusterName, task.ContainerInstanceID, msg.TaskID, task.ReservedCPU, task.ReservedMemoryMiB)
	}
	return nil
}

// releaseReservation returns a stopped task's capacity to its instance under CAS.
func (s *Service) releaseReservation(kv nats.KeyValue, cluster, instanceID, taskID string, cpu, mem int) error {
	for range reservePlacementRetries {
		entry, err := kv.Get(InstanceKey(cluster, instanceID))
		if err != nil {
			return nil //nolint:nilerr // instance gone; nothing to release
		}
		var rec InstanceRecord
		if uerr := json.Unmarshal(entry.Value(), &rec); uerr != nil {
			return uerr
		}
		rec.ReservedCPU = max(rec.ReservedCPU-cpu, 0)
		rec.ReservedMemoryMiB = max(rec.ReservedMemoryMiB-mem, 0)
		rec.PlacedTasks = slices.DeleteFunc(rec.PlacedTasks, func(v string) bool { return v == taskID })
		data, merr := json.Marshal(&rec)
		if merr != nil {
			return merr
		}
		if _, uerr := kv.Update(InstanceKey(cluster, instanceID), data, entry.Revision()); uerr == nil {
			return nil
		}
	}
	return nil
}
