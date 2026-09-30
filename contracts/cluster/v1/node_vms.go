package clusterv1

const (
	// NodeVMsSubject fans out a request for each responding daemon's VM
	// inventory. Its reply payload is NodeVMsResponse.
	NodeVMsSubject = "spinifex.node.vms"
)

// VMGPUInfo describes one GPU attached to a VM in a NodeVMsResponse.
type VMGPUInfo struct {
	Model      string `json:"model"`
	VRAMMiB    int64  `json:"vram_mib"`
	PCIAddress string `json:"pci_address,omitempty"` // whole-GPU passthrough only
	Profile    string `json:"profile,omitempty"`     // MIG profile name; empty for whole-GPU
	MdevPath   string `json:"mdev_path,omitempty"`   // MIG only
}

// VMInfo describes one VM in a node's inventory response.
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
	ManagedBy string      `json:"managed_by,omitempty"`
	GPUs      []VMGPUInfo `json:"gpus,omitempty"`
	// Health is a display label for instance health: "ok", "impaired",
	// "recovering", or "-" for non-running VMs. CrashCount is the lifetime
	// crash tally within the current restart window.
	Health     string `json:"health,omitempty"`
	CrashCount int    `json:"crash_count,omitempty"`
}

// NodeVMsResponse is one daemon's reply on NodeVMsSubject. The subject fans
// out, so callers collect one response per responding node.
type NodeVMsResponse struct {
	Node string   `json:"node"`
	Host string   `json:"host"`
	VMs  []VMInfo `json:"vms"`
}
