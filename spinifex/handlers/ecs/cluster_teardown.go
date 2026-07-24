package handlers_ecs

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// clusterKeyPrefix is the KV subtree holding all of a cluster's records.
func clusterKeyPrefix(cluster string) string {
	return "clusters/" + cluster + "/"
}

// DeleteCluster tears a cluster down and sweeps its KV subtree. Unlike AWS (which
// rejects a non-empty cluster), Mulga cascades so terraform destroy round-trips
// regardless of teardown ordering: every non-STOPPED task is force-stopped
// (releasing ENIs, deregistering TG targets, returning capacity), every service
// is marked INACTIVE, then the whole clusters/{name}/ prefix is deleted. Returns
// the cluster with Status INACTIVE.
func (s *Service) DeleteCluster(ctx context.Context, input *ecs.DeleteClusterInput, accountID string) (*ecs.DeleteClusterOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var rec ClusterRecord
	found, err := getJSON(ctx, kv, ClusterMetaKey(cluster), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	tasks, err := s.listTaskRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		s.forceStopTask(ctx, kv, accountID, &tasks[i], "Cluster deleted")
	}

	if err := deleteKeysWithPrefix(ctx, kv, clusterKeyPrefix(cluster)); err != nil {
		return nil, err
	}

	rec.Status = ClusterStatusInactive
	return &ecs.DeleteClusterOutput{Cluster: rec.toAWS()}, nil
}

// DeregisterContainerInstance removes a container instance from a cluster. With
// Force set it first force-stops the instance's non-STOPPED tasks; without Force
// it rejects an instance that still has running tasks (AWS parity). The instance
// record and its assignment inbox are deleted; the response carries Status
// INACTIVE.
func (s *Service) DeregisterContainerInstance(ctx context.Context, input *ecs.DeregisterContainerInstanceInput, accountID string) (*ecs.DeregisterContainerInstanceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	id := containerInstanceShortID(aws.StringValue(input.ContainerInstance))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var rec InstanceRecord
	found, err := getJSON(ctx, kv, InstanceKey(cluster, id), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}

	active, err := s.instanceActiveTasks(ctx, kv, cluster, id)
	if err != nil {
		return nil, err
	}
	if len(active) > 0 && !aws.BoolValue(input.Force) {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	for i := range active {
		s.forceStopTask(ctx, kv, accountID, &active[i], "Container instance deregistered")
	}

	if derr := deleteKeysWithPrefix(ctx, kv, AssignmentsPrefix(cluster, id)); derr != nil {
		return nil, derr
	}
	if derr := kv.Delete(ctx, InstanceKey(cluster, id)); derr != nil {
		return nil, derr
	}
	rec.Status = ClusterStatusInactive
	return &ecs.DeregisterContainerInstanceOutput{ContainerInstance: s.instanceToAWS(&rec)}, nil
}

// UpdateContainerInstancesState sets the requested instances ACTIVE or DRAINING.
// Draining force-stops the instance's service-owned tasks so the reconciler
// relaunches them elsewhere; standalone (non-service) tasks are left running,
// matching AWS. Unknown instances surface as Failures.
func (s *Service) UpdateContainerInstancesState(ctx context.Context, input *ecs.UpdateContainerInstancesStateInput, accountID string) (*ecs.UpdateContainerInstancesStateOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	status := aws.StringValue(input.Status)
	if status != InstanceStatusActive && status != InstanceStatusDraining {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.UpdateContainerInstancesStateOutput{}
	for _, ref := range awsStringSlice(input.ContainerInstances) {
		id := containerInstanceShortID(ref)
		var rec InstanceRecord
		found, gerr := getJSON(ctx, kv, InstanceKey(cluster, id), &rec)
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
			continue
		}
		rec.Status = status
		rec.Reaped = false
		if perr := putJSON(ctx, kv, InstanceKey(cluster, id), &rec); perr != nil {
			return nil, perr
		}
		if status == InstanceStatusDraining {
			s.drainInstanceServiceTasks(ctx, kv, accountID, cluster, id)
		}
		out.ContainerInstances = append(out.ContainerInstances, s.instanceToAWS(&rec))
	}
	return out, nil
}

// instanceActiveTasks returns a cluster's non-STOPPED tasks placed on instanceID.
func (s *Service) instanceActiveTasks(ctx context.Context, kv jetstream.KeyValue, cluster, instanceID string) ([]TaskRecord, error) {
	all, err := s.listTaskRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecord, 0, len(all))
	for _, t := range all {
		if t.ContainerInstanceID == instanceID && t.LastStatus != TaskStatusStopped {
			out = append(out, t)
		}
	}
	return out, nil
}

// drainInstanceServiceTasks force-stops the instance's service-owned tasks on an
// intentional DRAINING; the service reconciler then relaunches them on another
// instance. Standalone tasks are left running (AWS DRAINING semantics).
func (s *Service) drainInstanceServiceTasks(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, instanceID string) {
	active, err := s.instanceActiveTasks(ctx, kv, cluster, instanceID)
	if err != nil {
		return
	}
	for i := range active {
		if serviceNameFromGroup(active[i].Group) == "" {
			continue
		}
		s.forceStopTask(ctx, kv, accountID, &active[i], "Container instance draining")
	}
}
