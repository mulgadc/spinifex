package bus

import "time"

// InstanceCapacity is a container instance's total registrable resources.
type InstanceCapacity struct {
	CPU       int `json:"cpu"`       // total vCPU units (1 vCPU = 1024)
	MemoryMiB int `json:"memoryMiB"` // total memory in MiB
}

// RegisterInstance is published on RegisterSubject when an ecs-agent boots and
// joins its cluster. The scheduler records a container-instance entry keyed by
// InstanceID (handlers_ecs.InstanceKey).
type RegisterInstance struct {
	AccountID    string           `json:"accountId"`
	ClusterName  string           `json:"clusterName"`
	InstanceID   string           `json:"instanceId"`
	AZ           string           `json:"availabilityZone"`
	Hostname     string           `json:"hostname"`
	Capacity     InstanceCapacity `json:"capacity"`
	AgentVersion string           `json:"agentVersion"`
	RegisteredAt time.Time        `json:"registeredAt"`
}

// Heartbeat is published on HeartbeatSubject every ~30s while the agent is
// healthy. A missed heartbeat past the scheduler's TTL marks the instance
// DISCONNECTED (wired in Sprint 4e).
type Heartbeat struct {
	AccountID    string    `json:"accountId"`
	ClusterName  string    `json:"clusterName"`
	InstanceID   string    `json:"instanceId"`
	Status       string    `json:"status"` // ACTIVE | DRAINING
	RunningTasks int       `json:"runningTasks"`
	SentAt       time.Time `json:"sentAt"`
}

// Instance status values reported in Heartbeat.Status.
const (
	StatusActive   = "ACTIVE"
	StatusDraining = "DRAINING"
)

// Task lifecycle status values, matching the AWS ECS task status enum, reported
// in TaskState.LastStatus and per-container in ContainerStatus.Status.
const (
	TaskStatusPending = "PENDING"
	TaskStatusRunning = "RUNNING"
	TaskStatusStopped = "STOPPED"
)

// PortMapping is a container port exposed on the host (bridge mode v1).
type PortMapping struct {
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"` // tcp | udp (default tcp)
}

// AssignContainer is one container the agent must pull and run for a task.
type AssignContainer struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	CPU       int    `json:"cpu,omitempty"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
	// GPU is the whole-GPU count requested via resourceRequirements type=GPU.
	// Device pinning/CDI injection is not wired yet (later Epic C tasks).
	GPU          int               `json:"gpu,omitempty"`
	Essential    bool              `json:"essential"`
	Command      []string          `json:"command,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	PortMappings []PortMapping     `json:"portMappings,omitempty"`
	// LogDriver is the requested container log driver; only json-file is honored
	// (host-side). Any other value means logs are discarded (warned at register).
	LogDriver string `json:"logDriver,omitempty"`
}

// Assign is published on AssignSubject when the scheduler places a task on this
// instance (scheduler → agent). The agent pulls each container image, creates and
// starts the containers via containerd, then reports progress on TaskStateSubject.
type Assign struct {
	AccountID       string            `json:"accountId"`
	ClusterName     string            `json:"clusterName"`
	InstanceID      string            `json:"instanceId"`
	TaskID          string            `json:"taskId"`
	TaskARN         string            `json:"taskArn"`
	TaskDefFamily   string            `json:"taskDefFamily"`
	TaskDefRevision int               `json:"taskDefRevision"`
	Containers      []AssignContainer `json:"containers"`
	// ENI* describe the awsvpc task ENI the scheduler already created + hot-plugged
	// into this instance. The agent finds the NIC by ENIMacAddress, moves it into
	// the task netns, and assigns ENIPrivateIP. Empty for bridge/host mode.
	ENIID         string `json:"eniId,omitempty"`
	ENIMacAddress string `json:"eniMac,omitempty"`
	ENIPrivateIP  string `json:"eniPrivateIp,omitempty"`
	ENISubnetID   string `json:"eniSubnetId,omitempty"`
	// TaskRoleARN is the task's IAM role (from the taskdef); when set, the agent
	// serves its credentials at 169.254.170.2 under CredID, defaulting CredID to
	// the taskID when the scheduler leaves it empty.
	CredID      string `json:"credId,omitempty"`
	TaskRoleARN string `json:"taskRoleArn,omitempty"`
	// ExecutionRoleARN is the task's execution role (from the taskdef); when set,
	// the agent assumes it to authorize ECR image pulls instead of the container-
	// instance role. Empty falls back to the instance role.
	ExecutionRoleARN string    `json:"executionRoleArn,omitempty"`
	AssignedAt       time.Time `json:"assignedAt"`
}

// StopDirective is one pending task-stop the scheduler places in an instance's
// stop inbox (scheduler → agent). The agent kills + reaps the task's containers,
// then reports STOPPED with Reason. Delivery is idempotent; the STOPPED
// transition deletes the inbox entry.
type StopDirective struct {
	TaskID string `json:"taskId"`
	Reason string `json:"reason,omitempty"`
}

// ContainerStatus is a single container's reported lifecycle state.
type ContainerStatus struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // PENDING | RUNNING | STOPPED
	ContainerID string `json:"containerId,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
}

// TaskState is published on TaskStateSubject as the agent drives a task through
// its lifecycle (agent → scheduler). The scheduler updates the task record and
// recomputes the instance's remaining capacity.
type TaskState struct {
	AccountID   string            `json:"accountId"`
	ClusterName string            `json:"clusterName"`
	InstanceID  string            `json:"instanceId"`
	TaskID      string            `json:"taskId"`
	LastStatus  string            `json:"lastStatus"` // PENDING | RUNNING | STOPPED
	Containers  []ContainerStatus `json:"containers,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	ReportedAt  time.Time         `json:"reportedAt"`
}
