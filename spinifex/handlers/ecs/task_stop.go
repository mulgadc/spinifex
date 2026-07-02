package handlers_ecs

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
)

// defaultStopReason is recorded when StopTask is called without a reason.
const defaultStopReason = "Task stopped by user"

// StopTask requests a cooperative stop: it marks the task DesiredStatus=STOPPED
// and posts a directive to the instance's stop inbox. The agent reaps the
// container and reports STOPPED, which drives the capacity + ENI + TG release in
// recordTaskState. LastStatus stays RUNNING until the container is actually reaped
// so a freed port is never rebound while the old container still holds it
// (ecs-v1.md:165).
func (s *Service) StopTask(input *ecs.StopTaskInput, accountID string) (*ecs.StopTaskOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	taskID := taskShortID(aws.StringValue(input.Task))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var task TaskRecord
	found, err := getJSON(kv, TaskKey(cluster, taskID), &task)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	reason := aws.StringValue(input.Reason)
	if reason == "" {
		reason = defaultStopReason
	}
	s.requestStopTask(kv, accountID, &task, reason)
	return &ecs.StopTaskOutput{Task: s.taskToAWS(accountID, &task)}, nil
}

// StartTask places one task per named container instance (explicit placement, no
// scheduler bin-pack). Mirrors RunTask's reserve → record → ENI → assign flow.
func (s *Service) StartTask(input *ecs.StartTaskInput, accountID string) (*ecs.StartTaskOutput, error) {
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

	mode := resolveNetworkMode(taskDef)
	netCfg := awsvpcConfigFromStartTask(input)
	if mode == NetworkModeAwsvpc && netCfg.firstSubnet() == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cpu, mem := taskDef.reservedCPU(), taskDef.reservedMemory()
	group := aws.StringValue(input.Group)
	startedBy := aws.StringValue(input.StartedBy)

	out := &ecs.StartTaskOutput{}
	for _, ref := range awsStringSlice(input.ContainerInstances) {
		instanceID := containerInstanceShortID(ref)
		taskID := uuid.NewString()
		inst, rerr := s.reserveOnInstance(kv, cluster, instanceID, taskID, cpu, mem)
		if rerr != nil {
			out.Failures = append(out.Failures, &ecs.Failure{
				Arn: aws.String(ref), Reason: aws.String("RESOURCE:placement"), Detail: aws.String(rerr.Error()),
			})
			continue
		}
		rec := s.newTaskRecord(accountID, cluster, taskID, taskDef, inst, cpu, mem)
		rec.NetworkMode = mode
		rec.Group = group
		rec.StartedBy = startedBy
		if mode == NetworkModeAwsvpc {
			if failure := s.provisionTaskENI(kv, accountID, cluster, rec, netCfg); failure != nil {
				out.Failures = append(out.Failures, failure)
				continue
			}
		}
		if err := putJSON(kv, TaskKey(cluster, taskID), rec); err != nil {
			return nil, err
		}
		if err := s.publishAssign(kv, accountID, cluster, inst.InstanceID, rec, taskDef); err != nil {
			slog.Error("ECS StartTask: failed to publish assign", "task", taskID, "instance", inst.InstanceID, "err", err)
		}
		out.Tasks = append(out.Tasks, s.taskToAWS(accountID, rec))
	}
	return out, nil
}

// reserveOnInstance commits a task's capacity reservation onto a specific
// container instance under a KV CAS. Unlike reservePlacement it does not bin-pack;
// the instance is fixed by the caller (StartTask).
func (s *Service) reserveOnInstance(kv nats.KeyValue, cluster, instanceID, taskID string, cpu, mem int) (*InstanceRecord, error) {
	for range reservePlacementRetries {
		entry, err := kv.Get(InstanceKey(cluster, instanceID))
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		if err != nil {
			return nil, err
		}
		var live InstanceRecord
		if uerr := json.Unmarshal(entry.Value(), &live); uerr != nil {
			return nil, uerr
		}
		if live.Status != InstanceStatusActive || !live.fits(cpu, mem) {
			return nil, errors.New("instance has insufficient capacity")
		}
		live.ReservedCPU += cpu
		live.ReservedMemoryMiB += mem
		live.PlacedTasks = append(live.PlacedTasks, taskID)
		data, merr := json.Marshal(&live)
		if merr != nil {
			return nil, merr
		}
		if _, uerr := kv.Update(InstanceKey(cluster, instanceID), data, entry.Revision()); uerr == nil {
			return &live, nil
		}
	}
	return nil, errors.New("reservation contended")
}

// requestStopTask is the cooperative stop used while the agent is alive: it marks
// the task DesiredStatus=STOPPED and posts a stop directive to the instance's stop
// inbox, but leaves LastStatus, capacity, ENI, targets, and the assign inbox
// untouched. The agent reaps the container and reports STOPPED; recordTaskState
// then performs the single release. No-op once the task is already STOPPED. A task
// with no container instance (never placed) is forced-stopped directly since no
// agent can report it.
func (s *Service) requestStopTask(kv nats.KeyValue, accountID string, task *TaskRecord, reason string) {
	if task.LastStatus == TaskStatusStopped {
		return
	}
	if task.ContainerInstanceID == "" {
		s.forceStopTask(kv, accountID, task, reason)
		return
	}
	task.DesiredStatus = TaskStatusStopped
	if perr := putJSON(kv, TaskKey(task.Cluster, task.TaskID), task); perr != nil {
		slog.Error("ECS requestStopTask: persist failed", "task", task.TaskID, "err", perr)
		return
	}
	sd := bus.StopDirective{TaskID: task.TaskID, Reason: reason}
	if perr := putJSON(kv, StopKey(task.Cluster, task.ContainerInstanceID, task.TaskID), &sd); perr != nil {
		slog.Error("ECS requestStopTask: post stop directive failed", "task", task.TaskID, "err", perr)
	}
}

// forceStopTask is the control-plane forced-stop: it transitions a task to
// STOPPED, releases its capacity + ENI, drains its assign inbox, and deregisters
// its ELBv2 targets. Used where no live agent can reap the container (heartbeat
// reaper, cluster/instance teardown). The mutated record is reflected back into
// task.
func (s *Service) forceStopTask(kv nats.KeyValue, accountID string, task *TaskRecord, reason string) {
	if task.LastStatus == TaskStatusStopped {
		return
	}
	now := time.Now().UTC()
	task.LastStatus = TaskStatusStopped
	task.DesiredStatus = TaskStatusStopped
	task.StoppedReason = reason
	task.StoppedAt = now
	// Release the auto-assigned EIP before persisting so the cleared public IP
	// lands in this single write.
	s.releaseTaskPublicIP(accountID, task)
	if perr := putJSON(kv, TaskKey(task.Cluster, task.TaskID), task); perr != nil {
		slog.Error("ECS forceStopTask: persist failed", "task", task.TaskID, "err", perr)
		return
	}
	s.deregisterServiceTargets(kv, accountID, task)
	s.reclaimAssignInbox(kv, task.Cluster, task.ContainerInstanceID, task.TaskID)
	s.reclaimStopInbox(kv, task.Cluster, task.ContainerInstanceID, task.TaskID)
	s.reclaimTaskENI(accountID, task)
	if rerr := s.releaseReservation(kv, task.Cluster, task.ContainerInstanceID, task.TaskID, task.ReservedCPU, task.ReservedMemoryMiB); rerr != nil {
		slog.Error("ECS forceStopTask: release reservation failed", "task", task.TaskID, "err", rerr)
	}
}

// awsvpcConfigFromStartTask extracts the awsvpc subnet/SG selection from a
// StartTask input (StartTask has no bin-pack, so no RunTaskInput to reuse).
func awsvpcConfigFromStartTask(input *ecs.StartTaskInput) awsvpcConfig {
	var cfg awsvpcConfig
	if input != nil && input.NetworkConfiguration != nil && input.NetworkConfiguration.AwsvpcConfiguration != nil {
		v := input.NetworkConfiguration.AwsvpcConfiguration
		cfg.Subnets = awsStringSlice(v.Subnets)
		cfg.SecurityGroups = awsStringSlice(v.SecurityGroups)
	}
	return cfg
}

// registerServiceTargets registers a task's awsvpc ENI IP with each of its
// owning service's target groups. No-op when the task is not a service task,
// the service has no load balancers, or the task has no ENI IP (bridge/host).
func (s *Service) registerServiceTargets(kv nats.KeyValue, accountID string, task *TaskRecord) {
	s.applyServiceTargets(kv, accountID, task, true)
}

// deregisterServiceTargets is the STOPPED-side inverse of registerServiceTargets.
func (s *Service) deregisterServiceTargets(kv nats.KeyValue, accountID string, task *TaskRecord) {
	s.applyServiceTargets(kv, accountID, task, false)
}

func (s *Service) applyServiceTargets(kv nats.KeyValue, accountID string, task *TaskRecord, register bool) {
	name := serviceNameFromGroup(task.Group)
	if name == "" || task.ENIPrivateIP == "" || s.targets == nil {
		return
	}
	var svc ServiceRecord
	found, err := getJSON(kv, ServiceKey(task.Cluster, name), &svc)
	if err != nil || !found || len(svc.LoadBalancers) == 0 {
		return
	}
	for _, lb := range svc.LoadBalancers {
		var terr error
		if register {
			terr = s.targets.Register(accountID, lb.TargetGroupARN, task.ENIPrivateIP, lb.ContainerPort)
		} else {
			terr = s.targets.Deregister(accountID, lb.TargetGroupARN, task.ENIPrivateIP, lb.ContainerPort)
		}
		if terr != nil {
			slog.Error("ECS target registration failed", "service", name, "tg", lb.TargetGroupARN,
				"register", register, "err", terr)
		}
	}
}

// assignTaskPublicIP allocates + associates an Elastic IP to a service task's
// ENI when the owning service has AssignPublicIp=ENABLED, then persists the
// public IP onto the task. No-op for non-service or non-awsvpc tasks, or when an
// EIP is already assigned. Best-effort: a failure logs and leaves the task with
// only its private endpoint.
func (s *Service) assignTaskPublicIP(kv nats.KeyValue, accountID string, task *TaskRecord) {
	if s.eips == nil || task.ENIID == "" || task.ENIPublicIP != "" {
		return
	}
	name := serviceNameFromGroup(task.Group)
	if name == "" {
		return
	}
	var svc ServiceRecord
	found, err := getJSON(kv, ServiceKey(task.Cluster, name), &svc)
	if err != nil || !found || !strings.EqualFold(svc.AssignPublicIP, "ENABLED") {
		return
	}
	publicIP, allocID, err := s.eips.AllocateAndAssociate(accountID, task.ENIID)
	if err != nil {
		slog.Error("ECS auto-EIP assign failed", "task", task.TaskID, "eni", task.ENIID, "err", err)
		return
	}
	task.ENIPublicIP = publicIP
	task.ENIEIPAllocationID = allocID
	if perr := putJSON(kv, TaskKey(task.Cluster, task.TaskID), task); perr != nil {
		slog.Error("ECS auto-EIP: persist public IP failed", "task", task.TaskID, "err", perr)
	}
}

// releaseTaskPublicIP disassociates + releases a task's auto-assigned Elastic IP
// and clears the record. No-op when no EIP was assigned. The cleared record is
// reflected back into task; callers on the STOPPED path persist it.
func (s *Service) releaseTaskPublicIP(accountID string, task *TaskRecord) {
	if s.eips == nil || task.ENIEIPAllocationID == "" {
		return
	}
	if err := s.eips.Release(accountID, task.ENIEIPAllocationID); err != nil {
		slog.Error("ECS auto-EIP release failed", "task", task.TaskID, "alloc", task.ENIEIPAllocationID, "err", err)
	}
	task.ENIPublicIP = ""
	task.ENIEIPAllocationID = ""
}
