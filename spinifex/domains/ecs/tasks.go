package ecs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/taskdefinition"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// reservePlacementRetries bounds the CAS retry loop when concurrent RunTask calls
// contend for the same instance's capacity.
const reservePlacementRetries = 5

// RunTask places input.Count tasks of a definition onto container instances via
// bin-pack, reserves their capacity in KV, writes PENDING task records, and
// publishes an assign on the Layer-2 bus for each. Placement failures for a task
// are returned as RunTask failures; already-placed tasks in the same call stay.
func (s *Service) RunTask(ctx context.Context, input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error) {
	if err := validateTags(input.Tags); err != nil {
		return nil, err
	}
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	var clusterRec ClusterRecord
	found, err := taskdefinition.GetJSON(ctx, kv, ClusterMetaKey(cluster), &clusterRec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	taskDef, err := s.resolveTaskDef(ctx, kv, aws.StringValue(input.TaskDefinition))
	if err != nil {
		return nil, err
	}

	count := int(aws.Int64Value(input.Count))
	if count <= 0 {
		count = 1
	}
	strategy := placementStrategyFromAWS(input.PlacementStrategy)
	cpu, mem, gpu := taskDef.ReservedCPU(), taskDef.ReservedMemory(), taskDef.ReservedGPU()

	mode := resolveNetworkMode(taskDef)
	netCfg, err := parseAwsvpcConfig(input, mode)
	if err != nil {
		return nil, err
	}
	eni := eniReservationFor(mode)

	out := &ecs.RunTaskOutput{Failures: []*ecs.Failure{}}
	for i := 0; i < count; i++ {
		taskID := uuid.NewV4().String()
		inst, err := s.reservePlacement(ctx, kv, cluster, taskID, cpu, mem, gpu, eni, strategy)
		if i == 0 && errors.Is(err, errNoContainerInstances) {
			// AWS refuses the whole request, before any task is recorded.
			return nil, awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "No Container Instances were found in your cluster.")
		}
		if err != nil {
			out.Failures = append(out.Failures, placementFailure(err))
			continue
		}

		rec := s.newTaskRecord(accountID, cluster, taskID, taskDef, inst, cpu, mem, gpu)
		rec.NetworkMode = mode
		rec.Group = aws.StringValue(input.Group)
		rec.StartedBy = aws.StringValue(input.StartedBy)
		rec.Tags = tagsToMap(input.Tags)
		if mode == NetworkModeAwsvpc {
			if failure := s.provisionTaskENI(ctx, kv, accountID, cluster, rec, netCfg); failure != nil {
				out.Failures = append(out.Failures, failure)
				continue
			}
		}

		if err := taskdefinition.PutJSON(ctx, kv, TaskKey(cluster, taskID), rec); err != nil {
			return nil, err
		}
		if err := s.publishAssign(ctx, kv, accountID, cluster, inst.InstanceID, rec, taskDef); err != nil {
			slog.ErrorContext(ctx, "ECS RunTask: failed to publish assign", "task", taskID, "instance", inst.InstanceID, "err", err)
		}
		out.Tasks = append(out.Tasks, s.taskToAWS(accountID, rec, taskDef))
	}
	return out, nil
}

// eniReservationFor reports how many task ENI slots a network mode costs an
// instance. Only awsvpc hot-plugs one; bridge and host share the instance's own.
func eniReservationFor(mode string) int {
	if mode == NetworkModeAwsvpc {
		return 1
	}
	return 0
}

// placementFailure converts a placement error into a RunTask failure, naming the
// resource that bound. A capacity refusal carries the reason alone, as AWS does:
// the reason already says which resource ran out, and the wrapped error's
// internal identifiers and server error codes belong in the log, where they are
// read by someone who can act on them.
func placementFailure(err error) *ecs.Failure {
	if errors.Is(err, ErrNoENICapacity) {
		return &ecs.Failure{Reason: aws.String(failureResourceENI)}
	}
	return &ecs.Failure{
		Reason: aws.String("RESOURCE:placement"),
		Detail: aws.String(err.Error()),
	}
}

// failureResourceENI is the AWS reason for a task declined for want of an
// awsvpc network interface. AWS spells the resource in upper case.
const failureResourceENI = "RESOURCE:ENI"

// provisionTaskENI allocates and hot-plugs an awsvpc task's ENI, stamping its
// identity onto rec. On any failure it rolls back (releases a half-allocated ENI
// and the placement reservation) and returns a RunTask failure for this task —
// the caller skips it without leaking the ENI or the reserved capacity.
//
// Placement has already reserved the instance's ENI slot, so reaching a failure
// here means something the reservation could not predict; the caller gets the
// resource name and the log gets the reason.
func (s *Service) provisionTaskENI(ctx context.Context, kv jetstream.KeyValue, accountID, cluster string, rec *TaskRecord, netCfg awsvpcConfig) *ecs.Failure {
	rollback := func(stage string, err error) *ecs.Failure {
		slog.ErrorContext(ctx, "ECS RunTask: task ENI "+stage+" failed",
			"task", rec.TaskID, "instance", rec.ContainerInstanceID, "eni", rec.ENIID, "err", err)
		if rerr := s.releaseReservation(ctx, kv, cluster, rec.ContainerInstanceID, rec.TaskID, rec.ReservedCPU, rec.ReservedMemoryMiB, rec.GPU, eniReservationFor(rec.NetworkMode)); rerr != nil {
			slog.ErrorContext(ctx, "ECS RunTask: reservation rollback failed", "task", rec.TaskID, "err", rerr)
		}
		return &ecs.Failure{
			Reason: aws.String(failureResourceENI),
			Detail: aws.String("could not provision a network interface for this task"),
		}
	}

	alloc, err := s.eni.Allocate(ctx, accountID, netCfg.firstSubnet(), netCfg.securityGroupPtrs())
	if err != nil {
		return rollback("allocate", err)
	}
	rec.ENIID = alloc.ENIID
	rec.ENIMacAddress = alloc.MacAddress
	rec.ENIPrivateIP = alloc.PrivateIP
	rec.ENISubnetID = alloc.SubnetID

	attachmentID, err := s.eni.Attach(ctx, accountID, rec.ContainerInstanceID, alloc.ENIID)
	rec.ENIAttachmentID = attachmentID
	if err != nil {
		s.reclaimTaskENI(ctx, accountID, rec)
		if rec.ENIReleased {
			// Release succeeded, so nothing is owed; RunTask's caller never
			// persists a failed placement, so clearing here is the end of it.
			rec.ENIMacAddress, rec.ENIPrivateIP, rec.ENISubnetID = "", "", ""
		} else {
			// Release failed: persist this record as STOPPED so the sweep
			// retries it like any other owed ENI, rather than losing the
			// only pointer when RunTask drops the record on the floor.
			rec.LastStatus = TaskStatusStopped
			rec.DesiredStatus = TaskStatusStopped
			rec.StoppedReason = fmt.Sprintf("ENI attach failed: %s", err)
			rec.StoppedAt = time.Now().UTC()
			if perr := taskdefinition.PutJSON(ctx, kv, TaskKey(cluster, rec.TaskID), rec); perr != nil {
				slog.ErrorContext(ctx, "ECS RunTask: persist ENI rollback failed", "task", rec.TaskID, "err", perr)
			}
		}
		return rollback("attach", err)
	}
	return nil
}

// reservePlacement bin-packs a task onto an ACTIVE instance and commits the
// reservation under a KV CAS, retrying on contention.
func (s *Service) reservePlacement(ctx context.Context, kv jetstream.KeyValue, cluster, taskID string, cpu, mem, gpu, eni int, strategy string) (*InstanceRecord, error) {
	for range reservePlacementRetries {
		instances, err := s.listInstanceRecords(ctx, kv, cluster)
		if err != nil {
			return nil, err
		}
		if len(instances) == 0 {
			return nil, errNoContainerInstances
		}
		chosen, err := placeTask(instances, cpu, mem, gpu, eni, strategy)
		if err != nil {
			return nil, err
		}

		// Re-read the chosen instance for its current revision, then CAS-update.
		entry, err := kv.Get(ctx, InstanceKey(cluster, chosen.InstanceID))
		if err != nil {
			continue
		}
		var live InstanceRecord
		if uerr := json.Unmarshal(entry.Value(), &live); uerr != nil {
			return nil, uerr
		}
		if !live.fits(cpu, mem, gpu, eni) {
			continue
		}
		live.ReservedCPU += cpu
		live.ReservedMemoryMiB += mem
		live.ReservedGPU += gpu
		live.ReservedENIs += eni
		live.PlacedTasks = append(live.PlacedTasks, taskID)
		data, merr := json.Marshal(&live)
		if merr != nil {
			return nil, merr
		}
		if _, uerr := kv.Update(ctx, InstanceKey(cluster, live.InstanceID), data, entry.Revision()); uerr != nil {
			continue // lost the CAS race; retry placement
		}
		return &live, nil
	}
	return nil, fmt.Errorf("placement contended after %d attempts", reservePlacementRetries)
}

// newTaskRecord builds a PENDING task record for a placed task.
func (s *Service) newTaskRecord(accountID, cluster, taskID string, td *taskdefinition.Record, inst *InstanceRecord, cpu, mem, gpu int) *TaskRecord {
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
		AvailabilityZone:     inst.AZ,
		DesiredStatus:        TaskStatusRunning,
		LastStatus:           TaskStatusPending,
		ReservedCPU:          cpu,
		ReservedMemoryMiB:    mem,
		GPU:                  gpu,
		CreatedAt:            now,
	}
	if td.TaskRoleArn != "" {
		rec.CredID = uuid.NewV4().String()
	}
	for _, c := range td.Containers {
		rec.Containers = append(rec.Containers, ContainerState{Name: c.Name, Status: TaskStatusPending})
	}
	return rec
}

// publishAssign writes the task assignment into the instance's KV inbox. The
// agent drains it by polling the gateway (PollAssignments) rather than
// subscribing to NATS, so the bus stays host-internal. Durable + restart-safe:
// an unacked assign survives an agent crash and is re-delivered on the next poll.
func (s *Service) publishAssign(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, instanceID string, rec *TaskRecord, td *taskdefinition.Record) error {
	msg := ecsv1.Assign{
		AccountID:        accountID,
		ClusterName:      cluster,
		InstanceID:       instanceID,
		TaskID:           rec.TaskID,
		TaskARN:          rec.ARN,
		TaskDefFamily:    td.Family,
		TaskDefRevision:  td.Revision,
		TaskRoleARN:      td.TaskRoleArn,
		CredID:           rec.CredID,
		ExecutionRoleARN: td.ExecutionRoleArn,
		ENIID:            rec.ENIID,
		ENIMacAddress:    rec.ENIMacAddress,
		ENIPrivateIP:     rec.ENIPrivateIP,
		ENISubnetID:      rec.ENISubnetID,
		AssignedAt:       time.Now().UTC(),
	}
	for _, c := range td.Containers {
		msg.Containers = append(msg.Containers, containerToAssign(c))
	}
	return taskdefinition.PutJSON(ctx, kv, AssignmentKey(cluster, instanceID, rec.TaskID), &msg)
}

// DescribeTasks returns task records for the named tasks in a cluster.
func (s *Service) DescribeTasks(ctx context.Context, input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error) {
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	includeTags := slices.Contains(awsStringSlice(input.Include), ecs.TaskFieldTags)
	out := &ecs.DescribeTasksOutput{Failures: []*ecs.Failure{}}
	for _, ref := range awsStringSlice(input.Tasks) {
		taskID := ContainerInstanceShortID(ref)
		var rec TaskRecord
		found, err := taskdefinition.GetJSON(ctx, kv, TaskKey(cluster, taskID), &rec)
		if err != nil {
			return nil, err
		}
		if found {
			td, err := taskDefOf(ctx, kv, &rec)
			if err != nil {
				return nil, err
			}
			t := s.taskToAWS(accountID, &rec, td)
			if !includeTags {
				t.Tags = []*ecs.Tag{}
			}
			out.Tasks = append(out.Tasks, t)
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// ListTasks returns the ARNs of the cluster's tasks matching the input filters.
// An omitted desiredStatus means RUNNING, as it does on AWS, so an unfiltered
// call does not hand back every task the cluster has ever run.
func (s *Service) ListTasks(ctx context.Context, input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error) {
	cluster := ClusterShortName(aws.StringValue(input.Cluster))
	req, err := parseListPage(input.MaxResults, input.NextToken, listPageMax, listPageMax)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var clusterRec ClusterRecord
	found, err := taskdefinition.GetJSON(ctx, kv, ClusterMetaKey(cluster), &clusterRec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}
	keys, err := taskdefinition.KeysWithPrefix(ctx, kv, TasksPrefix(cluster))
	if err != nil {
		return nil, err
	}
	filter := newTaskFilter(input)
	arns := make([]string, 0, len(keys))
	for _, k := range keys {
		var rec TaskRecord
		found, err := taskdefinition.GetJSON(ctx, kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found && filter.matches(&rec) {
			arns = append(arns, rec.ARN)
		}
	}
	page, next := paging.EC2Page(arns, identity, req)
	return &ecs.ListTasksOutput{TaskArns: aws.StringSlice(page), NextToken: next}, nil
}

// taskFilter is ListTasks' input reduced to the fields a task record can be
// matched on. An empty field matches everything; desiredStatus is the one that
// defaults, because AWS narrows an unfiltered call to RUNNING.
type taskFilter struct {
	desiredStatus     string
	family            string
	startedBy         string
	serviceGroup      string
	containerInstance string
}

func newTaskFilter(input *ecs.ListTasksInput) taskFilter {
	f := taskFilter{desiredStatus: TaskStatusRunning}
	if input == nil {
		return f
	}
	if v := aws.StringValue(input.DesiredStatus); v != "" {
		f.desiredStatus = strings.ToUpper(v)
	}
	f.family = aws.StringValue(input.Family)
	f.startedBy = aws.StringValue(input.StartedBy)
	if v := aws.StringValue(input.ServiceName); v != "" {
		f.serviceGroup = serviceTaskGroup(ServiceShortName(v))
	}
	if v := aws.StringValue(input.ContainerInstance); v != "" {
		f.containerInstance = ContainerInstanceShortID(v)
	}
	return f
}

// matches reports whether a task record satisfies every set filter. Desired
// status is compared against the record's own DesiredStatus rather than its
// last reported one, so a task on its way down is still listed as stopping.
func (f taskFilter) matches(rec *TaskRecord) bool {
	if f.desiredStatus != "" && rec.DesiredStatus != f.desiredStatus {
		return false
	}
	if f.family != "" && rec.TaskDefFamily != f.family {
		return false
	}
	if f.startedBy != "" && rec.StartedBy != f.startedBy {
		return false
	}
	if f.serviceGroup != "" && rec.Group != f.serviceGroup {
		return false
	}
	if f.containerInstance != "" && rec.ContainerInstanceID != f.containerInstance {
		return false
	}
	return true
}

// taskDefOf loads the revision a task was launched from, or nil when the
// revision has since been deleted.
func taskDefOf(ctx context.Context, kv jetstream.KeyValue, r *TaskRecord) (*taskdefinition.Record, error) {
	var td taskdefinition.Record
	found, err := taskdefinition.GetJSON(ctx, kv, taskdefinition.RevKey(r.TaskDefFamily, r.TaskDefRevision), &td)
	if err != nil || !found {
		return nil, err
	}
	return &td, nil
}

// taskToAWS projects a task record. td is the revision it was launched from;
// when nil, the members it alone holds (container image, cpu, memory) are left
// out. Members only the agent could report are not modelled.
func (s *Service) taskToAWS(accountID string, r *TaskRecord, td *taskdefinition.Record) *ecs.Task {
	group := r.Group
	if group == "" {
		group = "family:" + r.TaskDefFamily
	}
	t := &ecs.Task{
		TaskArn:              aws.String(r.ARN),
		ClusterArn:           aws.String(ClusterARN(s.region, accountID, r.Cluster)),
		TaskDefinitionArn:    aws.String(r.TaskDefARN),
		DesiredStatus:        aws.String(r.DesiredStatus),
		LastStatus:           aws.String(r.LastStatus),
		ContainerInstanceArn: aws.String(r.ContainerInstanceARN),
		Tags:                 tagsToAWS(r.Tags),
		CreatedAt:            awsTimeOrNil(r.CreatedAt),
		StartedAt:            awsTimeOrNil(r.StartedAt),
		StoppedAt:            awsTimeOrNil(r.StoppedAt),
		Group:                aws.String(group),
		LaunchType:           aws.String(ecs.LaunchTypeEc2),
		EnableExecuteCommand: aws.Bool(false),
		Overrides: &ecs.TaskOverride{
			ContainerOverrides:            []*ecs.ContainerOverride{},
			InferenceAcceleratorOverrides: []*ecs.InferenceAcceleratorOverride{},
		},
	}
	if r.AvailabilityZone != "" {
		t.AvailabilityZone = aws.String(r.AvailabilityZone)
	}
	cpu, mem := strconv.Itoa(r.ReservedCPU), strconv.Itoa(r.ReservedMemoryMiB)
	if td != nil && td.CPU != "" {
		cpu = td.CPU
	}
	if td != nil && td.Memory != "" {
		mem = td.Memory
	}
	if cpu != "0" {
		t.Cpu = aws.String(cpu)
	}
	if mem != "0" {
		t.Memory = aws.String(mem)
	}
	if r.StartedBy != "" {
		t.StartedBy = aws.String(r.StartedBy)
	}
	if r.StoppedReason != "" {
		t.StoppedReason = aws.String(r.StoppedReason)
	}
	if r.StopCode != "" {
		t.StopCode = aws.String(r.StopCode)
	}
	t.StoppingAt = awsTimeOrNil(r.StoppingAt)
	t.ExecutionStoppedAt = awsTimeOrNil(r.ExecutionStoppedAt)
	if r.ENIID != "" {
		att := &ecs.Attachment{
			Id:     aws.String(r.ENIAttachmentID),
			Type:   aws.String("ElasticNetworkInterface"),
			Status: aws.String(taskENIAttachmentStatus(r)),
			Details: []*ecs.KeyValuePair{
				{Name: aws.String("networkInterfaceId"), Value: aws.String(r.ENIID)},
				{Name: aws.String("privateIPv4Address"), Value: aws.String(r.ENIPrivateIP)},
				{Name: aws.String("macAddress"), Value: aws.String(r.ENIMacAddress)},
				{Name: aws.String("subnetId"), Value: aws.String(r.ENISubnetID)},
			},
		}
		if r.ENIPublicIP != "" {
			att.Details = append(att.Details,
				&ecs.KeyValuePair{Name: aws.String("publicIPv4Address"), Value: aws.String(r.ENIPublicIP)})
		}
		t.Attachments = append(t.Attachments, att)
	}
	for _, c := range r.Containers {
		t.Overrides.ContainerOverrides = append(t.Overrides.ContainerOverrides, &ecs.ContainerOverride{Name: aws.String(c.Name)})
		ctr := &ecs.Container{
			Name:         aws.String(c.Name),
			LastStatus:   aws.String(c.Status),
			TaskArn:      aws.String(r.ARN),
			ContainerArn: aws.String(ContainerARN(s.region, accountID, r.Cluster, r.TaskID, c.Name)),
		}
		if def := td.Container(c.Name); def != nil {
			ctr.Image = aws.String(def.Image)
			ctr.Cpu = aws.String(strconv.Itoa(def.CPU))
			if def.MemoryMiB > 0 {
				ctr.Memory = aws.String(strconv.Itoa(def.MemoryMiB))
			}
		}
		if c.ExitCode != nil {
			ctr.ExitCode = aws.Int64(int64(*c.ExitCode))
		}
		if len(c.GPUIDs) > 0 {
			ctr.GpuIds = aws.StringSlice(c.GPUIDs)
		}
		t.Containers = append(t.Containers, ctr)
	}
	return t
}

// ENI attachment states a task reports, from AWS's attachment lifecycle.
const (
	attachmentStatusPrecreated = "PRECREATED"
	attachmentStatusAttached   = "ATTACHED"
	attachmentStatusDetaching  = "DETACHING"
	attachmentStatusDeleted    = "DELETED"
)

// taskENIAttachmentStatus derives the attachment state from the ENI's own
// progress: plumbed into the task once the agent reports it started, detaching
// while a stopped task's release is still owed, deleted once released.
func taskENIAttachmentStatus(r *TaskRecord) string {
	switch {
	case r.ENIReleased:
		return attachmentStatusDeleted
	case r.LastStatus == TaskStatusStopped:
		return attachmentStatusDetaching
	case r.ENIAttachmentID != "" && !r.StartedAt.IsZero():
		return attachmentStatusAttached
	default:
		return attachmentStatusPrecreated
	}
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
