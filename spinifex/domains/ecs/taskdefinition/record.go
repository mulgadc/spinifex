// Package taskdefinition owns the ECS task definition: its revision records,
// revision allocation, reference resolution and deregistration.
package taskdefinition

import (
	"time"
)

// Status values of a task definition revision.
const (
	StatusActive   = "ACTIVE"
	StatusInactive = "INACTIVE"
)

// Container is the persisted subset of an ecs.ContainerDefinition needed to
// pull and run a container (bridge mode v1).
type Container struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	CPU       int    `json:"cpu,omitempty"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
	// GPU is the whole-GPU count from a resourceRequirements entry of type GPU
	// (AWS ECS semantics; the value is a stringified integer). Device pinning and
	// placement accounting land in later Epic C tasks.
	GPU       int      `json:"gpu,omitempty"`
	Essential bool     `json:"essential"`
	Command   []string `json:"command,omitempty"`
	// Environment carries no omitempty so a caller-supplied empty collection
	// stays distinguishable from an absent one: nil marshals to null and empty
	// to {}, and describe re-emits whichever was stored.
	Environment  map[string]string `json:"environment"`
	PortMappings []PortMapping     `json:"portMappings,omitempty"`
	// LogDriver / LogOptions capture the container's logConfiguration. Only the
	// host-side json-file default is honored; any other driver is accepted for
	// parity but warned at register time (logs are discarded).
	LogDriver  string            `json:"logDriver,omitempty"`
	LogOptions map[string]string `json:"logOptions,omitempty"`
	// User is enforced via oci.WithUser. Empty is indistinguishable from unset
	// (both mean "run as the image's default user"), so a plain string is enough.
	User string `json:"user,omitempty"`
	// ReadonlyRootFilesystem, Privileged, PseudoTerminal and Interactive are
	// pointers because the per-value fail-open test requires telling "caller
	// said false" from "caller said nothing" apart: a nil field is omitted on
	// describe (unchanged from before this fix), a non-nil field is echoed and
	// enforced exactly as submitted, true or false.
	ReadonlyRootFilesystem *bool `json:"readonlyRootFilesystem,omitempty"`
	Privileged             *bool `json:"privileged,omitempty"`
	PseudoTerminal         *bool `json:"pseudoTerminal,omitempty"`
	Interactive            *bool `json:"interactive,omitempty"`
	// SystemControls are sysctl namespace/value pairs applied to the OCI spec.
	// No omitempty, for the same presence reason as Environment.
	SystemControls []SystemControl `json:"systemControls"`
	// MountPointsSet and VolumesFromSet record that the caller supplied an empty
	// collection. A non-empty one is refused at registration, so only presence
	// needs storing for describe to return [] instead of omitting the field.
	MountPointsSet bool `json:"mountPointsSet,omitempty"`
	VolumesFromSet bool `json:"volumesFromSet,omitempty"`
	// InitProcessEnabled is stored only when supplied false; true is refused at
	// registration, so a stored value is always false.
	InitProcessEnabled *bool `json:"initProcessEnabled,omitempty"`
	// CapAdd / CapDrop are linuxParameters.capabilities.add/drop. The rest of
	// linuxParameters (devices, sharedMemorySize, tmpfs) is refused at
	// registration rather than stored, see the register validation in the ECS adapter.
	CapAdd  []string `json:"capAdd,omitempty"`
	CapDrop []string `json:"capDrop,omitempty"`
	// StartTimeout / StopTimeout are pointers because zero is a meaningful value
	// distinct from unset on the AWS shape; enforcement lives in the agent's
	// task lifecycle, not in the OCI spec.
	StartTimeout *int64 `json:"startTimeout,omitempty"`
	StopTimeout  *int64 `json:"stopTimeout,omitempty"`
}

// RuntimePlatform is the persisted subset of an ecs.RuntimePlatform.
// Pure echo, same as RequiresCompatibilities: nothing selects capacity by CPU
// architecture or OS family, but a caller that sets it must read it back.
type RuntimePlatform struct {
	CPUArchitecture       string `json:"cpuArchitecture,omitempty"`
	OperatingSystemFamily string `json:"operatingSystemFamily,omitempty"`
}

// Record is the persisted task definition revision at RevKey.
type Record struct {
	Family           string `json:"family"`
	Revision         int    `json:"revision"`
	ARN              string `json:"arn"`
	NetworkMode      string `json:"networkMode,omitempty"`
	CPU              string `json:"cpu,omitempty"`
	Memory           string `json:"memory,omitempty"`
	TaskRoleArn      string `json:"taskRoleArn,omitempty"`
	ExecutionRoleArn string `json:"executionRoleArn,omitempty"`
	// Persisted purely so Describe echoes back what Register was given. Only
	// the EC2 launch type is implemented, but a client that sets this and
	// reads back an empty list sees permanent drift.
	RequiresCompatibilities []string          `json:"requiresCompatibilities,omitempty"`
	RuntimePlatform         *RuntimePlatform  `json:"runtimePlatform,omitempty"`
	Containers              []Container       `json:"containers"`
	Status                  string            `json:"status"`
	Tags                    map[string]string `json:"tags,omitempty"`
	RegisteredAt            time.Time         `json:"registeredAt"`
}

// ReservedCPU sums the per-container CPU reservations used for bin-pack
// placement. A taskdef-level cpu/memory is not modelled in v1; placement uses
// the container sums.
func (t *Record) ReservedCPU() int {
	total := 0
	for _, c := range t.Containers {
		total += c.CPU
	}
	return total
}

// ReservedMemory sums the per-container memory reservations, as ReservedCPU.
func (t *Record) ReservedMemory() int {
	total := 0
	for _, c := range t.Containers {
		total += c.MemoryMiB
	}
	return total
}

// ReservedGPU sums the task definition's per-container whole-GPU counts.
// It is the task-level total carried onto the task record and the bus assign.
func (t *Record) ReservedGPU() int {
	total := 0
	for _, c := range t.Containers {
		total += c.GPU
	}
	return total
}

// PortMapping is a container port exposed on the host (bridge mode v1). Name
// is the Service Connect port-mapping name, stored only to echo on describe.
type PortMapping struct {
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Name          string `json:"name,omitempty"`
}

// SystemControl is a sysctl namespace/value pair (linux systemControls).
type SystemControl struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}
