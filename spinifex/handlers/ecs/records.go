package handlers_ecs

import (
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// Lifecycle status constants matching the AWS ECS enums. Task statuses are
// re-exported from the bus package so the scheduler and the wire payloads agree.
const (
	ClusterStatusActive   = "ACTIVE"
	ClusterStatusInactive = "INACTIVE"

	InstanceStatusActive   = "ACTIVE"
	InstanceStatusDraining = "DRAINING"

	TaskDefStatusActive   = "ACTIVE"
	TaskDefStatusInactive = "INACTIVE"

	TaskStatusPending = bus.TaskStatusPending
	TaskStatusRunning = bus.TaskStatusRunning
	TaskStatusStopped = bus.TaskStatusStopped

	ServiceStatusActive   = "ACTIVE"
	ServiceStatusDraining = "DRAINING"
	ServiceStatusInactive = "INACTIVE"

	// SchedulingStrategyReplica is the only strategy supported in v1 (Q15);
	// DAEMON is rejected at CreateService.
	SchedulingStrategyReplica = "REPLICA"
	SchedulingStrategyDaemon  = "DAEMON"
)

// ARN builders for the ECS resource shapes (ecs-v1.md §1). Region + accountID
// scope every ARN; the partition is fixed to "aws" to match the rest of the
// stack.
func ClusterARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", region, accountID, name)
}

func TaskDefARN(region, accountID, family string, rev int) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/%s:%d", region, accountID, family, rev)
}

func TaskARN(region, accountID, cluster, taskID string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:task/%s/%s", region, accountID, cluster, taskID)
}

func ContainerInstanceARN(region, accountID, cluster, ciID string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:container-instance/%s/%s", region, accountID, cluster, ciID)
}

func ServiceARN(region, accountID, cluster, name string) string {
	return fmt.Sprintf("arn:aws:ecs:%s:%s:service/%s/%s", region, accountID, cluster, name)
}

// serviceTaskGroup is the AWS task-group label a service stamps on its tasks
// ("service:{name}"). The reconciler counts a service's tasks by this group and
// the task-state hook resolves a task back to its owning service through it.
func serviceTaskGroup(name string) string {
	return "service:" + name
}

// serviceNameFromGroup returns the service name encoded in a task group, or ""
// when the group is not a service group.
func serviceNameFromGroup(group string) string {
	const p = "service:"
	if len(group) > len(p) && group[:len(p)] == p {
		return group[len(p):]
	}
	return ""
}

// ClusterRecord is the persisted cluster meta at ClusterMetaKey.
type ClusterRecord struct {
	Name      string            `json:"name"`
	ARN       string            `json:"arn"`
	Status    string            `json:"status"`
	Tags      map[string]string `json:"tags,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
}

// ContainerDef is the persisted subset of an ecs.ContainerDefinition needed to
// pull and run a container (bridge mode v1).
type ContainerDef struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	CPU          int               `json:"cpu,omitempty"`
	MemoryMiB    int               `json:"memoryMiB,omitempty"`
	Essential    bool              `json:"essential"`
	Command      []string          `json:"command,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	PortMappings []bus.PortMapping `json:"portMappings,omitempty"`
}

// TaskDefRecord is the persisted task definition revision at TaskDefRevKey.
type TaskDefRecord struct {
	Family       string         `json:"family"`
	Revision     int            `json:"revision"`
	ARN          string         `json:"arn"`
	NetworkMode  string         `json:"networkMode,omitempty"`
	CPU          string         `json:"cpu,omitempty"`
	Memory       string         `json:"memory,omitempty"`
	TaskRoleArn  string         `json:"taskRoleArn,omitempty"`
	Containers   []ContainerDef `json:"containers"`
	Status       string         `json:"status"`
	RegisteredAt time.Time      `json:"registeredAt"`
}

// reservedCPU/reservedMemory sum the task definition's per-container reservations
// used for bin-pack placement. A taskdef-level cpu/memory is not modelled in v1;
// placement uses the container sums.
func (t *TaskDefRecord) reservedCPU() int {
	total := 0
	for _, c := range t.Containers {
		total += c.CPU
	}
	return total
}

func (t *TaskDefRecord) reservedMemory() int {
	total := 0
	for _, c := range t.Containers {
		total += c.MemoryMiB
	}
	return total
}

// InstanceRecord is the persisted container-instance state at InstanceKey. The
// scheduler writes it from the Layer-2 bus (register/heartbeat) and reserves
// capacity by appending placed task IDs.
type InstanceRecord struct {
	InstanceID     string `json:"instanceId"`
	ARN            string `json:"arn"`
	Cluster        string `json:"cluster"`
	AZ             string `json:"availabilityZone,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
	Status         string `json:"status"`
	TotalCPU       int    `json:"totalCpu"`
	TotalMemoryMiB int    `json:"totalMemoryMiB"`
	// ReservedCPU/ReservedMemoryMiB track capacity committed to placed tasks;
	// placement increments them under a KV CAS and the task-state STOPPED path
	// releases them. Remaining = Total - Reserved.
	ReservedCPU       int       `json:"reservedCpu"`
	ReservedMemoryMiB int       `json:"reservedMemoryMiB"`
	AgentVersion      string    `json:"agentVersion,omitempty"`
	PlacedTasks       []string  `json:"placedTasks,omitempty"`
	RegisteredAt      time.Time `json:"registeredAt"`
	LastSeen          time.Time `json:"lastSeen"`
	// Reaped marks a DRAINING caused by the heartbeat reaper (involuntary), as
	// opposed to an operator UpdateContainerInstancesState drain. A reaped
	// instance is restored to ACTIVE when its agent re-registers; an operator
	// drain persists.
	Reaped bool `json:"reaped,omitempty"`
}

// ContainerState is a container's last-reported status within a task record.
type ContainerState struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	ContainerID string `json:"containerId,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
}

// TaskRecord is the persisted task state at TaskKey; source of truth for
// DescribeTasks and the placement/capacity accounting.
type TaskRecord struct {
	TaskID  string `json:"taskId"`
	ARN     string `json:"arn"`
	Cluster string `json:"cluster"`
	// Group / StartedBy mirror the AWS task fields. A service's tasks carry
	// Group="service:{name}" so the reconciler counts them and the task-state
	// hook resolves a RUNNING/STOPPED task back to its owning service.
	Group                string           `json:"group,omitempty"`
	StartedBy            string           `json:"startedBy,omitempty"`
	TaskDefFamily        string           `json:"taskDefFamily"`
	TaskDefRevision      int              `json:"taskDefRevision"`
	TaskDefARN           string           `json:"taskDefArn"`
	ContainerInstanceID  string           `json:"containerInstanceId,omitempty"`
	ContainerInstanceARN string           `json:"containerInstanceArn,omitempty"`
	DesiredStatus        string           `json:"desiredStatus"`
	LastStatus           string           `json:"lastStatus"`
	StoppedReason        string           `json:"stoppedReason,omitempty"`
	ReservedCPU          int              `json:"reservedCpu"`
	ReservedMemoryMiB    int              `json:"reservedMemoryMiB"`
	Containers           []ContainerState `json:"containers,omitempty"`
	// NetworkMode is the resolved task network mode (awsvpc|bridge|host). The
	// STOPPED path consults it to decide whether an ENI must be reclaimed.
	NetworkMode string `json:"networkMode,omitempty"`
	// ENI* hold the per-task elastic network interface for awsvpc mode, allocated
	// by the scheduler at placement and reclaimed at STOPPED. Empty otherwise.
	ENIID           string `json:"eniId,omitempty"`
	ENIAttachmentID string `json:"eniAttachmentId,omitempty"`
	ENIPrivateIP    string `json:"eniPrivateIp,omitempty"`
	ENIMacAddress   string `json:"eniMac,omitempty"`
	ENISubnetID     string `json:"eniSubnetId,omitempty"`
	// ENIPublicIP / ENIEIPAllocationID hold the auto-assigned Elastic IP for an
	// awsvpc task whose service has AssignPublicIp=ENABLED. Set on the RUNNING
	// transition and released on STOPPED. Empty otherwise.
	ENIPublicIP        string    `json:"eniPublicIp,omitempty"`
	ENIEIPAllocationID string    `json:"eniEipAllocationId,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	StartedAt          time.Time `json:"startedAt,omitzero"`
	StoppedAt          time.Time `json:"stoppedAt,omitzero"`
}

// LoadBalancerTarget is one ELBv2 target-group attachment on a service. On task
// RUNNING the scheduler registers the task's ENI IP at ContainerPort; on STOPPED
// it deregisters (ecs-v1.md Q8, single-writer).
type LoadBalancerTarget struct {
	TargetGroupARN string `json:"targetGroupArn"`
	ContainerName  string `json:"containerName,omitempty"`
	ContainerPort  int    `json:"containerPort"`
}

// ServiceRecord is the persisted service state at ServiceKey. The captured
// network config + placement strategy let the reconciler launch replacement
// tasks identically to the original RunTask.
type ServiceRecord struct {
	Name               string               `json:"name"`
	ARN                string               `json:"arn"`
	Cluster            string               `json:"cluster"`
	TaskDefFamily      string               `json:"taskDefFamily"`
	TaskDefRevision    int                  `json:"taskDefRevision"`
	TaskDefARN         string               `json:"taskDefArn"`
	DesiredCount       int                  `json:"desiredCount"`
	Status             string               `json:"status"`
	SchedulingStrategy string               `json:"schedulingStrategy"`
	LaunchType         string               `json:"launchType,omitempty"`
	NetworkMode        string               `json:"networkMode,omitempty"`
	Subnets            []string             `json:"subnets,omitempty"`
	SecurityGroups     []string             `json:"securityGroups,omitempty"`
	AssignPublicIP     string               `json:"assignPublicIp,omitempty"`
	PlacementStrategy  string               `json:"placementStrategy,omitempty"`
	LoadBalancers      []LoadBalancerTarget `json:"loadBalancers,omitempty"`
	DeploymentID       string               `json:"deploymentId"`
	RunningCount       int                  `json:"runningCount"`
	PendingCount       int                  `json:"pendingCount"`
	CreatedAt          time.Time            `json:"createdAt"`
	UpdatedAt          time.Time            `json:"updatedAt"`
}
