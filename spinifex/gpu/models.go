package gpu

type modelInfo struct {
	name      string
	memoryMiB int64
}

// knownModels maps "vendorID:deviceID" to human-readable model info.
// Used as a fallback when nvidia-smi/rocm-smi is unavailable.
var knownModels = map[string]modelInfo{
	// NVIDIA datacenter
	"10de:2236": {"NVIDIA A10", 23028},
	"10de:20b2": {"NVIDIA A100 SXM 40GB", 40960},
	"10de:20b5": {"NVIDIA A100 SXM 80GB", 81920},
	"10de:20f1": {"NVIDIA A100 PCIe 40GB", 40960},
	"10de:20f3": {"NVIDIA A100 PCIe 80GB", 81920},
	"10de:2331": {"NVIDIA H100 SXM", 81920},
	"10de:2330": {"NVIDIA H100 PCIe", 81920},
	"10de:233a": {"NVIDIA H100 NVL", 94208},
	"10de:2233": {"NVIDIA A30", 24576},
	"10de:26b5": {"NVIDIA L40", 46068},
	"10de:26b9": {"NVIDIA L40S", 46068},
	// NVIDIA consumer
	"10de:2684": {"NVIDIA GeForce RTX 4090", 24576},
	"10de:2782": {"NVIDIA GeForce RTX 4070", 12288},
	"10de:2204": {"NVIDIA GeForce RTX 3090", 24576},
	// AMD datacenter
	"1002:7448": {"AMD Instinct MI300X", 196608},
	"1002:740c": {"AMD Instinct MI250X", 65536},
	// AMD consumer
	"1002:744c": {"AMD Radeon RX 7900 XTX", 24576},
	"1002:73bf": {"AMD Radeon RX 6900 XT", 16384},
}

// computeModels is the set of "vendorID:deviceID" keys for datacenter/compute-only
// GPUs that have no display output and must use x-vga=off for QEMU passthrough.
// Consumer GPUs not in this set default to x-vga=on.
var computeModels = map[string]bool{
	// NVIDIA datacenter
	"10de:2236": true, // A10
	"10de:20b2": true, // A100 SXM 40GB
	"10de:20b5": true, // A100 SXM 80GB
	"10de:20f1": true, // A100 PCIe 40GB
	"10de:20f3": true, // A100 PCIe 80GB
	"10de:2331": true, // H100 SXM
	"10de:2330": true, // H100 PCIe
	"10de:233a": true, // H100 NVL
	"10de:2233": true, // A30
	"10de:26b5": true, // L40
	"10de:26b9": true, // L40S
	// AMD datacenter
	"1002:7448": true, // MI300X
	"1002:740c": true, // MI250X
}

// IsComputeGPU reports whether vendorID:deviceID identifies a headless compute GPU
// that should use x-vga=off in QEMU. Consumer/display GPUs return false.
// Both IDs must be lowercase hex without a "0x" prefix.
func IsComputeGPU(vendorID, deviceID string) bool {
	return computeModels[vendorID+":"+deviceID]
}

// lookupModel returns the model name and VRAM for a known vendorID:deviceID pair.
// Both IDs are expected as lowercase hex without a "0x" prefix.
// Returns zero values if the pair is not in the table.
func lookupModel(vendorID, deviceID string) (name string, memoryMiB int64) {
	if info, ok := knownModels[vendorID+":"+deviceID]; ok {
		return info.name, info.memoryMiB
	}
	return "", 0
}
