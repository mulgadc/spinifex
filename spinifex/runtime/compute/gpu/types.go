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
	MIGCapable     bool   // GPU model supports MIG partitioning
	MIGEnabled     bool   // MIG mode is currently active on this GPU
}

// MIGProfile describes one NVIDIA MIG partitioning profile supported by a GPU.
type MIGProfile struct {
	ID        int    // nvidia-smi profile ID
	Name      string // e.g. "1g.10gb"
	MemoryMiB int64
}

// MIGInstance describes one active MIG slice (GI + CI pair) on a physical GPU.
// Each instance surfaces as an mdev device and can be attached to exactly one VM.
type MIGInstance struct {
	GIID     int    // GPU Instance ID assigned by the driver
	CIID     int    // Compute Instance ID within the GI
	UUID     string // mdev UUID; stable while MIG mode remains active
	MdevPath string // /sys/bus/mdev/devices/<uuid>
	Profile  MIGProfile
}

// IOMMUGroupMember describes one PCI device within an IOMMU group.
type IOMMUGroupMember struct {
	PCIAddress string
	VendorID   string
	DeviceID   string
	Class      string // e.g. "0x030200"
}
