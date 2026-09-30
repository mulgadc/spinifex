package clusterv1

const (
	// NodeStatusSubject fans out a request for each responding daemon's status
	// and schedulable-capacity snapshot. Its reply payload is NodeStatusResponse.
	NodeStatusSubject = "spinifex.node.status"
)

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

// NodeStatusResponse is one daemon's reply on NodeStatusSubject. Schedulable
// capacity = TotalVCPU - ReservedVCPU - AllocVCPU (and the same for memory).
// The subject fans out, so callers collect one response per responding node.
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
	NATSRole string `json:"nats_role,omitempty"` // "leader" or "follower"

	// Never populated: predastore's meta nodes expose no status surface. Kept so
	// the JSON contract and the UI are unchanged when the probe is restored.
	PredastoreRole string `json:"predastore_role,omitempty"`

	// OVN DB Raft roles ("leader"/"follower"/""); empty when the node is not an
	// OVN DB cluster member or OVN is standalone.
	OVNNBRole string `json:"ovn_nb_role,omitempty"`
	OVNSBRole string `json:"ovn_sb_role,omitempty"`
}

// InstanceTypeCap describes available capacity for one instance type on a node.
type InstanceTypeCap struct {
	Name      string  `json:"name"`
	VCPU      int     `json:"vcpu"`
	MemoryGB  float64 `json:"memory_gb"`
	Available int     `json:"available"`
}
