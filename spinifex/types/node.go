package types

// NodeDiscoverResponse is the response for node discovery requests.
type NodeDiscoverResponse struct {
	Node string `json:"node"`
}

// NodeStatusResponse is returned by the spinifex.node.status NATS topic (fan-out).
//
// ReservedVCPU / ReservedMemGB are held back from guest scheduling for the
// spinifex daemon and co-located services. Schedulable capacity is
// TotalVCPU - ReservedVCPU - AllocVCPU (and the equivalent for memory);
// per-type counts in InstanceTypes already account for the reserve.
type NodeStatusResponse struct {
	Node           string            `json:"node"`
	Status         string            `json:"status"`
	Host           string            `json:"host"`
	Region         string            `json:"region"`
	AZ             string            `json:"az"`
	Uptime         int64             `json:"uptime"`
	Services       []string          `json:"services"`
	TotalVCPU      int               `json:"total_vcpu"`
	TotalMemGB     float64           `json:"total_mem_gb"`
	ReservedVCPU   int               `json:"reserved_vcpu"`
	ReservedMemGB  float64           `json:"reserved_mem_gb"`
	AllocVCPU      int               `json:"alloc_vcpu"`
	AllocMemGB     float64           `json:"alloc_mem_gb"`
	TotalGPUs      int               `json:"total_gpus"`
	AllocGPUs      int               `json:"alloc_gpus"`
	GPUCapable     bool              `json:"gpu_capable,omitempty"`
	GPUPassthrough bool              `json:"gpu_passthrough,omitempty"`
	GPUModels      []string          `json:"gpu_models,omitempty"`
	VMCount        int               `json:"vm_count"`
	InstanceTypes  []InstanceTypeCap `json:"instance_types"`

	// Leader roles for clustered services (empty string = service not running or not clustered)
	NATSRole       string `json:"nats_role,omitempty"`       // "leader" or "follower"
	PredastoreRole string `json:"predastore_role,omitempty"` // "leader" or "follower"
}

// InstanceTypeCap describes available capacity for one instance type on a node.
type InstanceTypeCap struct {
	Name      string  `json:"name"`
	VCPU      int     `json:"vcpu"`
	MemoryGB  float64 `json:"memory_gb"`
	Available int     `json:"available"`
}

// VMInfo describes a single VM for the cluster stats CLI.
type VMInfo struct {
	InstanceID   string  `json:"instance_id"`
	Status       string  `json:"status"`
	InstanceType string  `json:"instance_type"`
	VCPU         int     `json:"vcpu"`
	MemoryGB     float64 `json:"memory_gb"`
	LaunchTime   int64   `json:"launch_time"`
	// ManagedBy is the Spinifex platform component that owns this VM
	// (e.g. "elbv2"). Empty for customer VMs. The UI uses this to filter
	// system-managed resources out of customer-facing listings.
	ManagedBy string `json:"managed_by,omitempty"`
}

// NodeVMsResponse is returned by the spinifex.node.vms NATS topic (fan-out).
type NodeVMsResponse struct {
	Node string   `json:"node"`
	Host string   `json:"host"`
	VMs  []VMInfo `json:"vms"`
}
