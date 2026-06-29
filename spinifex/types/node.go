package types

// NodeDiscoverResponse is the response for node discovery requests.
type NodeDiscoverResponse struct {
	Node string `json:"node"`
}

// GPUSliceInfo describes a single MIG slice within a physical GPU.
type GPUSliceInfo struct {
	GIID       int    `json:"gi_id"`
	Profile    string `json:"profile"`
	VRAMMiB    int64  `json:"vram_mib"`
	MdevPath   string `json:"mdev_path"`
	InstanceID string `json:"instance_id,omitempty"`
}

// GPUInfo describes a physical GPU on a node and its current allocation state.
// For MIG GPUs, Slices lists each carved slice. For whole-GPU passthrough,
// Slices is nil and InstanceID identifies the claiming VM (empty if free).
type GPUInfo struct {
	PCIAddress string         `json:"pci_address"`
	Model      string         `json:"model"`
	VRAMMiB    int64          `json:"vram_mib"`
	MIGEnabled bool           `json:"mig_enabled"`
	MIGProfile string         `json:"mig_profile,omitempty"`
	InstanceID string         `json:"instance_id,omitempty"`
	Slices     []GPUSliceInfo `json:"slices,omitempty"`
}

// VMGPUInfo describes the GPU attached to a VM.
type VMGPUInfo struct {
	Model      string `json:"model"`
	VRAMMiB    int64  `json:"vram_mib"`
	PCIAddress string `json:"pci_address,omitempty"` // whole-GPU passthrough only
	Profile    string `json:"profile,omitempty"`     // MIG profile name; empty for whole-GPU
	MdevPath   string `json:"mdev_path,omitempty"`   // MIG only
}

// NodeStatusResponse is returned by the spinifex.node.status NATS topic (fan-out).
// Schedulable capacity = TotalVCPU - ReservedVCPU - AllocVCPU (same for memory).
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
	GPUs           []GPUInfo         `json:"gpus,omitempty"`
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
	ManagedBy string     `json:"managed_by,omitempty"`
	GPU       *VMGPUInfo `json:"gpu,omitempty"`
	// Health is a display label for instance health: "ok", "impaired",
	// "recovering", or "-" for non-running VMs. CrashCount is the lifetime
	// crash tally within the current restart window.
	Health     string `json:"health,omitempty"`
	CrashCount int    `json:"crash_count,omitempty"`
}

// NodeVMsResponse is returned by the spinifex.node.vms NATS topic (fan-out).
type NodeVMsResponse struct {
	Node string   `json:"node"`
	Host string   `json:"host"`
	VMs  []VMInfo `json:"vms"`
}
