package handlers_ecs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go/jetstream"
)

// listInstanceRecords returns all container-instance records in a cluster.
func (s *Service) listInstanceRecords(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]InstanceRecord, error) {
	keys, err := keysWithPrefix(ctx, kv, InstancesPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]InstanceRecord, 0, len(keys))
	for _, k := range keys {
		var rec InstanceRecord
		found, err := getJSON(ctx, kv, k, &rec)
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
func (s *Service) RegisterContainerInstance(ctx context.Context, input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error) {
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	instanceID := aws.StringValue(input.InstanceIdentityDocument)
	if instanceID == "" {
		instanceID = "ci-" + time.Now().UTC().Format("20060102150405")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, err := s.upsertInstance(ctx, kv, accountID, cluster, instanceID, func(r *InstanceRecord) {
		for _, res := range input.TotalResources {
			switch aws.StringValue(res.Name) {
			case "CPU":
				r.TotalCPU = int(aws.Int64Value(res.IntegerValue))
			case "MEMORY":
				r.TotalMemoryMiB = int(aws.Int64Value(res.IntegerValue))
			case "GPU":
				// AWS reports GPU as a STRINGSET of device UUIDs; the count is the
				// capacity, the UUIDs are the placeholder inventory (Epic C3 populates
				// them for real from the agent's nvidia-smi discovery).
				r.GPUIDs = aws.StringValueSlice(res.StringSetValue)
				r.TotalGPU = len(r.GPUIDs)
			}
		}
		// An attribute the agent did not discover is absent rather than blank, so
		// an empty value never overwrites what a previous registration reported.
		if t := attributeValue(input.Attributes, AttrInstanceType); t != "" {
			r.InstanceType = t
		}
		if az := attributeValue(input.Attributes, AttrAvailabilityZone); az != "" {
			r.AZ = az
		}
		if len(input.Tags) > 0 {
			r.Tags = tagsToMap(input.Tags)
		}
		// The agent heartbeats by re-registering. A re-register from a reaped
		// (involuntarily drained) instance proves the agent is back, so restore
		// it to ACTIVE. An operator drain (Reaped=false) is left untouched.
		if r.Status == InstanceStatusDraining && r.Reaped {
			r.Status = InstanceStatusActive
			r.Reaped = false
		}
	})
	if err != nil {
		return nil, err
	}
	counts, err := s.instanceTaskCounts(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	return &ecs.RegisterContainerInstanceOutput{ContainerInstance: s.instanceToAWS(rec, counts[instanceID])}, nil
}

// DescribeContainerInstances returns records for the named container instances.
func (s *Service) DescribeContainerInstances(ctx context.Context, input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error) {
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	counts, err := s.instanceTaskCounts(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeContainerInstancesOutput{}
	for _, ref := range awsStringSlice(input.ContainerInstances) {
		id := ContainerInstanceShortID(ref)
		var rec InstanceRecord
		found, err := getJSON(ctx, kv, InstanceKey(cluster, id), &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.ContainerInstances = append(out.ContainerInstances, s.instanceToAWS(&rec, counts[id]))
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// instanceListStatuses are the values the status filter accepts. A registration
// here is only ever ACTIVE or DRAINING; the other three are states AWS passes a
// registration through, so they are valid to ask for and match nothing.
var instanceListStatuses = map[string]bool{
	InstanceStatusActive:   true,
	InstanceStatusDraining: true,
	"REGISTERING":          true,
	"DEREGISTERING":        true,
	"REGISTRATION_FAILED":  true,
}

// ListContainerInstances returns the ARNs of the container instances in a
// cluster, narrowed to one registration status when the caller names one.
func (s *Service) ListContainerInstances(ctx context.Context, input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error) {
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	status := aws.StringValue(input.Status)
	if status != "" && !instanceListStatuses[status] {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	recs, err := s.listInstanceRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	arns := make([]string, 0, len(recs))
	for i := range recs {
		// The record's status is what DescribeContainerInstances reports, so a
		// filtered list and a describe of what it returns cannot disagree.
		if status != "" && recs[i].Status != status {
			continue
		}
		arns = append(arns, recs[i].ARN)
	}
	return &ecs.ListContainerInstancesOutput{ContainerInstanceArns: aws.StringSlice(arns)}, nil
}

// upsertInstance reads-or-creates the instance record, applies mutate, and writes
// it back. Used by both the AWS register path and the bus register handler.
func (s *Service) upsertInstance(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, instanceID string, mutate func(*InstanceRecord)) (*InstanceRecord, error) {
	var rec InstanceRecord
	found, err := getJSON(ctx, kv, InstanceKey(cluster, instanceID), &rec)
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
	if err := putJSON(ctx, kv, InstanceKey(cluster, instanceID), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// taskCounts is one container instance's live task tally.
type taskCounts struct {
	running int64
	pending int64
}

// instanceTaskCounts tallies each instance's RUNNING and PENDING tasks from the
// task records themselves. PlacedTasks cannot answer this: it is the list of
// tasks still owing capacity, so a task that stopped without its reservation
// being released stays in it and is counted as running forever.
func (s *Service) instanceTaskCounts(ctx context.Context, kv jetstream.KeyValue, cluster string) (map[string]taskCounts, error) {
	tasks, err := s.listTaskRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]taskCounts, len(tasks))
	for i := range tasks {
		id := tasks[i].ContainerInstanceID
		if id == "" {
			continue
		}
		c := counts[id]
		switch tasks[i].LastStatus {
		case TaskStatusRunning:
			c.running++
		case TaskStatusPending:
			c.pending++
		}
		counts[id] = c
	}
	return counts, nil
}

func (s *Service) instanceToAWS(r *InstanceRecord, counts taskCounts) *ecs.ContainerInstance {
	registered := []*ecs.Resource{
		{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalCPU))},
		{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalMemoryMiB))},
	}
	remaining := []*ecs.Resource{
		{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalCPU - r.ReservedCPU))},
		{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(r.TotalMemoryMiB - r.ReservedMemoryMiB))},
	}
	// GPU is only reported for GPU-capable instances (AWS parity); the STRINGSET
	// values are the device UUIDs when known, empty pending the agent's Epic C3
	// report-back — TotalGPU/ReservedGPU remain the authoritative counts either way.
	if r.TotalGPU > 0 {
		registered = append(registered, &ecs.Resource{
			Name: aws.String("GPU"), Type: aws.String("STRINGSET"), StringSetValue: aws.StringSlice(r.GPUIDs),
		})
		remaining = append(remaining, &ecs.Resource{
			Name: aws.String("GPU"), Type: aws.String("STRINGSET"), StringSetValue: aws.StringSlice(r.remainingGPUIDs()),
		})
	}
	return &ecs.ContainerInstance{
		ContainerInstanceArn: aws.String(r.ARN),
		Ec2InstanceId:        aws.String(r.InstanceID),
		Status:               aws.String(r.Status),
		AgentConnected:       aws.Bool(r.Status == InstanceStatusActive),
		RunningTasksCount:    aws.Int64(counts.running),
		PendingTasksCount:    aws.Int64(counts.pending),
		RegisteredResources:  registered,
		RemainingResources:   remaining,
		VersionInfo:          &ecs.VersionInfo{AgentVersion: aws.String(r.AgentVersion)},
		Attributes:           instanceAttributes(r),
		Tags:                 tagsToAWS(r.Tags),
	}
}

// Built-in container-instance attribute names, spelled as AWS spells them.
const (
	AttrInstanceType     = "ecs.instance-type"
	AttrAvailabilityZone = "ecs.availability-zone"
)

// attributeValue returns the value of the named attribute, or "" when the caller
// did not report it.
func attributeValue(attrs []*ecs.Attribute, name string) string {
	for _, a := range attrs {
		if a != nil && aws.StringValue(a.Name) == name {
			return aws.StringValue(a.Value)
		}
	}
	return ""
}

// instanceAttributes projects the built-in attributes back to a caller. Only
// what the record actually holds is reported, so an unknown instance type reads
// as absent rather than as an instance type of "".
func instanceAttributes(r *InstanceRecord) []*ecs.Attribute {
	var attrs []*ecs.Attribute
	add := func(name, value string) {
		if value == "" {
			return
		}
		attrs = append(attrs, &ecs.Attribute{Name: aws.String(name), Value: aws.String(value)})
	}
	add(AttrInstanceType, r.InstanceType)
	add(AttrAvailabilityZone, r.AZ)
	return attrs
}

// --- Layer-2 bus event handlers (called by the scheduler) ---

// recordRegister upserts a container-instance record from a bus RegisterInstance.
func (s *Service) recordRegister(ctx context.Context, msg *bus.RegisterInstance) error {
	kv, err := s.bucket(ctx, msg.AccountID)
	if err != nil {
		return err
	}
	_, err = s.upsertInstance(ctx, kv, msg.AccountID, msg.ClusterName, msg.InstanceID, func(r *InstanceRecord) {
		r.AZ = msg.AZ
		r.Hostname = msg.Hostname
		r.AgentVersion = msg.AgentVersion
		r.TotalCPU = msg.Capacity.CPU
		r.TotalMemoryMiB = msg.Capacity.MemoryMiB
		r.TotalGPU = msg.Capacity.GPU
		r.GPUIDs = msg.Capacity.GPUIDs
		r.Status = InstanceStatusActive
	})
	return err
}

// recordHeartbeat refreshes an instance's LastSeen and status from a bus beat.
func (s *Service) recordHeartbeat(ctx context.Context, msg *bus.Heartbeat) error {
	kv, err := s.bucket(ctx, msg.AccountID)
	if err != nil {
		return err
	}
	var rec InstanceRecord
	found, err := getJSON(ctx, kv, InstanceKey(msg.ClusterName, msg.InstanceID), &rec)
	if err != nil || !found {
		return err
	}
	rec.LastSeen = time.Now().UTC()
	if msg.Status != "" {
		rec.Status = msg.Status
	}
	return putJSON(ctx, kv, InstanceKey(msg.ClusterName, msg.InstanceID), &rec)
}

// recordTaskState applies an agent task-state report: it updates the task record
// and, on STOPPED, releases the reserved capacity back to the instance.
func (s *Service) recordTaskState(ctx context.Context, msg *bus.TaskState) error {
	kv, err := s.bucket(ctx, msg.AccountID)
	if err != nil {
		return err
	}
	var task TaskRecord
	found, err := getJSON(ctx, kv, TaskKey(msg.ClusterName, msg.TaskID), &task)
	if err != nil || !found {
		return err
	}

	prev := task.LastStatus
	task.LastStatus = msg.LastStatus
	if len(msg.Containers) > 0 {
		// Preserve pinned GPU UUIDs across a state change that omits them — the
		// STOPPED report carries no GPUIDs and would otherwise wipe the device
		// IDs the RUNNING report set, leaving DescribeTasks gpuIds empty.
		prevGPUIDs := make(map[string][]string, len(task.Containers))
		for _, c := range task.Containers {
			if len(c.GPUIDs) > 0 {
				prevGPUIDs[c.Name] = c.GPUIDs
			}
		}
		task.Containers = task.Containers[:0]
		for _, c := range msg.Containers {
			gpuIDs := c.GPUIDs
			if len(gpuIDs) == 0 {
				gpuIDs = prevGPUIDs[c.Name]
			}
			task.Containers = append(task.Containers, ContainerState{
				Name: c.Name, Status: c.Status, ContainerID: c.ContainerID, ExitCode: c.ExitCode, GPUIDs: gpuIDs,
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
		// A task that exits on its own was never asked to stop, so its desired
		// status is still RUNNING here. AWS moves it to STOPPED, and a caller
		// filtering on desired status would otherwise see it as still wanted.
		task.DesiredStatus = TaskStatusStopped
	}
	if err := putJSON(ctx, kv, TaskKey(msg.ClusterName, msg.TaskID), &task); err != nil {
		return err
	}

	// Register ELBv2 targets and assign a public IP on the transition into
	// RUNNING (Q8). assignTaskPublicIP persists the EIP onto the task itself.
	if msg.LastStatus == TaskStatusRunning && prev != TaskStatusRunning {
		s.registerServiceTargets(ctx, kv, msg.AccountID, &task)
		s.assignTaskPublicIP(ctx, kv, msg.AccountID, &task)
	}

	// Deregister targets, release the public IP, release capacity + reclaim the
	// task ENI once, on the transition into STOPPED.
	if msg.LastStatus == TaskStatusStopped && prev != TaskStatusStopped {
		// A task that stops without ever reaching RUNNING is a deployment failure;
		// feed the owning deployment's circuit breaker.
		if task.StartedAt.IsZero() {
			s.recordDeploymentFailure(ctx, kv, msg.ClusterName, &task)
		}
		s.deregisterServiceTargets(ctx, kv, msg.AccountID, &task)
		s.releaseTaskPublicIP(ctx, msg.AccountID, &task)
		s.reclaimAssignInbox(ctx, kv, msg.ClusterName, task.ContainerInstanceID, msg.TaskID)
		s.reclaimStopInbox(ctx, kv, msg.ClusterName, task.ContainerInstanceID, msg.TaskID)
		s.reclaimTaskENI(ctx, msg.AccountID, &task)
		if perr := putJSON(ctx, kv, TaskKey(msg.ClusterName, msg.TaskID), &task); perr != nil {
			slog.ErrorContext(ctx, "ECS task STOPPED: persist after EIP release failed", "task", msg.TaskID, "err", perr)
		}
		return s.releaseReservation(ctx, kv, msg.ClusterName, task.ContainerInstanceID, msg.TaskID, task.ReservedCPU, task.ReservedMemoryMiB, task.GPU, eniReservationFor(task.NetworkMode))
	}
	return nil
}

// SubmitTaskStateChange is the AWS-API task-state path (agent → gateway → here).
// It maps the SDK input onto the same bus.TaskState shape the Layer-2 bus
// delivers and converges on recordTaskState, so a gateway-routed agent reports
// state without touching NATS. The account is authoritative from accountID.
func (s *Service) SubmitTaskStateChange(ctx context.Context, input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error) {
	msg := bus.TaskState{
		AccountID:   accountID,
		ClusterName: ClusterShortName(aws.StringValue(input.Cluster)),
		TaskID:      TaskShortID(aws.StringValue(input.Task)),
		LastStatus:  aws.StringValue(input.Status),
		Reason:      aws.StringValue(input.Reason),
		ReportedAt:  time.Now().UTC(),
	}
	for _, c := range input.Containers {
		cs := bus.ContainerStatus{
			Name:        aws.StringValue(c.ContainerName),
			Status:      aws.StringValue(c.Status),
			ContainerID: aws.StringValue(c.RuntimeId),
		}
		if c.ExitCode != nil {
			code := int(aws.Int64Value(c.ExitCode))
			cs.ExitCode = &code
		}
		msg.Containers = append(msg.Containers, cs)
	}
	if err := s.recordTaskState(ctx, &msg); err != nil {
		return nil, err
	}
	// A task changing state is what moves a service's running count, so the
	// scheduler is woken here rather than left to find out on its own deadline.
	// The worker serving this call is rarely the leader, hence a published wake.
	if err := s.nc.Publish(bus.ServiceReconcileSubject(accountID, msg.ClusterName), nil); err != nil {
		slog.Debug("ECS: reconcile wake failed", "cluster", msg.ClusterName, "err", err)
	}
	return &ecs.SubmitTaskStateChangeOutput{Acknowledgment: aws.String("OK")}, nil
}

// recordDeploymentFailure increments the failed-task counter on the deployment
// that launched a task which stopped before ever running, driving the service's
// deployment circuit breaker. No-op for a non-service task or an unknown deployment.
func (s *Service) recordDeploymentFailure(ctx context.Context, kv jetstream.KeyValue, cluster string, task *TaskRecord) {
	name := serviceNameFromGroup(task.Group)
	depID := deploymentIDFromStartedBy(task.StartedBy)
	if name == "" || depID == "" {
		return
	}
	var svc ServiceRecord
	found, err := getJSON(ctx, kv, ServiceKey(cluster, name), &svc)
	if err != nil || !found {
		return
	}
	for i := range svc.Deployments {
		if svc.Deployments[i].ID == depID {
			svc.Deployments[i].FailedTasks++
			svc.Deployments[i].UpdatedAt = time.Now().UTC()
			if perr := putJSON(ctx, kv, ServiceKey(cluster, name), &svc); perr != nil {
				slog.ErrorContext(ctx, "ECS deployment failure accounting: persist failed", "service", name, "err", perr)
			}
			return
		}
	}
}

// releaseReservation returns a stopped task's capacity to its instance under CAS.
func (s *Service) releaseReservation(ctx context.Context, kv jetstream.KeyValue, cluster, instanceID, taskID string, cpu, mem, gpu, eni int) error {
	for range reservePlacementRetries {
		entry, err := kv.Get(ctx, InstanceKey(cluster, instanceID))
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var rec InstanceRecord
		if uerr := json.Unmarshal(entry.Value(), &rec); uerr != nil {
			return uerr
		}
		rec.ReservedCPU = max(rec.ReservedCPU-cpu, 0)
		rec.ReservedMemoryMiB = max(rec.ReservedMemoryMiB-mem, 0)
		rec.ReservedGPU = max(rec.ReservedGPU-gpu, 0)
		rec.ReservedENIs = max(rec.ReservedENIs-eni, 0)
		rec.PlacedTasks = slices.DeleteFunc(rec.PlacedTasks, func(v string) bool { return v == taskID })
		data, merr := json.Marshal(&rec)
		if merr != nil {
			return merr
		}
		if _, uerr := kv.Update(ctx, InstanceKey(cluster, instanceID), data, entry.Revision()); uerr == nil {
			return nil
		}
	}
	return nil
}
