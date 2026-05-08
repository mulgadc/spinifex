package gpu

// Vendor identifies the GPU manufacturer.
type Vendor string

const (
	VendorNVIDIA  Vendor = "nvidia"
	VendorAMD     Vendor = "amd"
	VendorIntel   Vendor = "intel"
	VendorUnknown Vendor = "unknown"
)

// GPUDevice describes a physical GPU discovered on the host.
type GPUDevice struct {
	PCIAddress     string // "0000:03:00.0"
	Vendor         Vendor
	VendorID       string // hex, e.g. "10de"
	DeviceID       string // hex, e.g. "2236"
	Model          string // e.g. "NVIDIA A10"
	MemoryMiB      int64  // VRAM in MiB; 0 if undiscoverable
	IOMMUGroup     int    // -1 if IOMMU not active
	OriginalDriver string // driver bound before VFIO takeover, e.g. "nvidia"
}

// IOMMUGroupMember describes one PCI device within an IOMMU group.
type IOMMUGroupMember struct {
	PCIAddress string
	VendorID   string
	DeviceID   string
	Class      string // e.g. "0x030200"
}
