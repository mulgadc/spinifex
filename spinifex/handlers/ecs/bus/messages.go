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
