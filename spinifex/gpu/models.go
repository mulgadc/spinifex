package gpu

type modelInfo struct {
	name      string
	memoryMiB int64
}

// knownModels maps "vendorID:deviceID" to human-readable model info.
// Used as a fallback when nvidia-smi/rocm-smi is unavailable.
//
// PCI IDs are checked against /usr/share/misc/pci.ids; do not add or change
// one without a matching line there. VRAM is not carried by pci.ids and must
// come from vendor documentation or an observed report, entry by entry.
//
// Ambiguity rule: several PCI IDs cover more than one card at different VRAM
// sizes (e.g. one Navi 31 ID spans four Radeon RX 7900 SKUs from 16 to 24GB).
// Record the SMALLEST VRAM among the covered cards. Under-reporting refuses
// an endpoint that would have fit, which an operator can see and override;
// over-reporting admits a model that then OOMs during weight load, on a host
// where the failure looks unrelated to the GPU table.
var knownModels = map[string]modelInfo{
	// NVIDIA datacenter (compute)
	"10de:2236": {"NVIDIA A10", 23028},
	"10de:2237": {"NVIDIA A10G", 23028}, // same GA102 silicon/board as A10
	"10de:2235": {"NVIDIA A40", 45634},
	"10de:20b0": {"NVIDIA A100 SXM4 40GB", 40960},
	"10de:20b2": {"NVIDIA A100 SXM4 80GB", 81920},
	"10de:20b5": {"NVIDIA A100 PCIe 80GB", 81920},
	"10de:20f1": {"NVIDIA A100 PCIe 40GB", 40960},
	"10de:20f3": {"NVIDIA A800-SXM4-80GB", 81920},
	"10de:20b7": {"NVIDIA A30 PCIe", 24576},
	"10de:25b6": {"NVIDIA A2 / A16", 15356}, // shared ID; both SKUs carry 16GB per physical GPU
	"10de:27b8": {"NVIDIA L4", 23034},
	"10de:1eb8": {"NVIDIA Tesla T4", 15109},
	"10de:2330": {"NVIDIA H100 SXM", 81920},
	"10de:2331": {"NVIDIA H100 PCIe", 81920},
	"10de:2337": {"NVIDIA H100 SXM5 64GB", 65536},
	"10de:2321": {"NVIDIA H100 NVL 94GB", 94208},
	"10de:233a": {"NVIDIA H800L 94GB", 94208},
	"10de:2335": {"NVIDIA H200 SXM", 144384},
	"10de:233b": {"NVIDIA H200 NVL", 144384},
	"10de:26b5": {"NVIDIA L40", 46068},
	"10de:26b9": {"NVIDIA L40S", 46068},
	"10de:2bb5": {"NVIDIA RTX Pro 6000 Blackwell Server Edition", 98304},
	// NVIDIA workstation (display output; not compute, not MIG)
	"10de:2233": {"NVIDIA RTX A5500", 24576},
	"10de:2230": {"NVIDIA RTX A6000", 49140},
	"10de:2231": {"NVIDIA RTX A5000", 24576},
	"10de:24b0": {"NVIDIA RTX A4000", 16376},
	"10de:2531": {"NVIDIA RTX A2000", 6138},
	"10de:2571": {"NVIDIA RTX A2000 12GB", 12288},
	"10de:25b0": {"NVIDIA RTX A1000", 8188}, // hardware-verified on wattle
	"10de:26b1": {"NVIDIA RTX 6000 Ada Generation", 49152},
	"10de:26b2": {"NVIDIA RTX 5000 Ada Generation", 32768},
	"10de:27b2": {"NVIDIA RTX 4000 Ada Generation", 20480},
	// NVIDIA consumer
	"10de:2684": {"NVIDIA GeForce RTX 4090", 24576},
	"10de:2782": {"NVIDIA GeForce RTX 4070 Ti", 12288},
	"10de:2204": {"NVIDIA GeForce RTX 3090", 24576},
	"10de:2203": {"NVIDIA GeForce RTX 3090 Ti", 24576},
	"10de:2208": {"NVIDIA GeForce RTX 3080 Ti", 12288},
	"10de:2206": {"NVIDIA GeForce RTX 3080", 10240},
	"10de:2487": {"NVIDIA GeForce RTX 3060", 12288},
	"10de:2486": {"NVIDIA GeForce RTX 3060 Ti", 8192},
	"10de:2484": {"NVIDIA GeForce RTX 3070", 8192},
	"10de:1e04": {"NVIDIA GeForce RTX 2080 Ti", 11264},
	// AMD datacenter (compute)
	"1002:75a0": {"AMD Instinct MI350X", 294896}, // absent from local pci.ids snapshot; left unverified per plan
	"1002:74a1": {"AMD Instinct MI300X", 196608},
	"1002:74a0": {"AMD Instinct MI300A", 131072},
	"1002:740c": {"AMD Instinct MI250X", 65536}, // per-GCD VRAM; card exposes 2 PCI devices, 64GB each
	"1002:740f": {"AMD Instinct MI210", 65536},
	"1002:738c": {"AMD Instinct MI100", 32768},
	// AMD workstation (display output; not compute)
	"1002:7448": {"AMD Radeon Pro W7900", 49152},
	"1002:745e": {"AMD Radeon Pro W7800", 32768},
	// AMD consumer
	"1002:744c": {"AMD Radeon RX 7900 XT/XTX/GRE/7900M", 16384}, // shared ID spans 16/16/20/24GB SKUs; smallest recorded
	"1002:73bf": {"AMD Radeon RX 6900 XT", 16384},
}

// computeModels is the set of "vendorID:deviceID" keys for datacenter/compute-only
// GPUs that have no display output and must use x-vga=off for QEMU passthrough.
// Consumer GPUs not in this set default to x-vga=on.
var computeModels = map[string]bool{
	// NVIDIA datacenter
	"10de:2236": true, // A10
	"10de:2237": true, // A10G
	"10de:2235": true, // A40
	"10de:20b0": true, // A100 SXM4 40GB
	"10de:20b2": true, // A100 SXM4 80GB
	"10de:20b5": true, // A100 PCIe 80GB
	"10de:20f1": true, // A100 PCIe 40GB
	"10de:20f3": true, // A800-SXM4-80GB
	"10de:20b7": true, // A30 PCIe
	"10de:25b6": true, // A2 / A16
	"10de:27b8": true, // L4
	"10de:1eb8": true, // Tesla T4
	"10de:2330": true, // H100 SXM
	"10de:2331": true, // H100 PCIe
	"10de:2337": true, // H100 SXM5 64GB
	"10de:2321": true, // H100 NVL 94GB
	"10de:233a": true, // H800L 94GB
	"10de:2335": true, // H200 SXM
	"10de:233b": true, // H200 NVL
	"10de:26b5": true, // L40
	"10de:26b9": true, // L40S
	"10de:2bb5": true, // RTX Pro 6000 Blackwell Server Edition
	// AMD datacenter
	"1002:75a0": true, // MI350X
	"1002:74a1": true, // MI300X
	"1002:74a0": true, // MI300A
	"1002:740c": true, // MI250X
	"1002:740f": true, // MI210
	"1002:738c": true, // MI100
}

// migCapableModels is the set of "vendorID:deviceID" keys for NVIDIA GPUs that
// support MIG partitioning. All are also in computeModels (headless datacenter cards).
// A10, A10G, L4, L40S and T4 are compute GPUs but do NOT support MIG.
var migCapableModels = map[string]bool{
	"10de:20b0": true, // A100 SXM4 40GB
	"10de:20b2": true, // A100 SXM4 80GB
	"10de:20b5": true, // A100 PCIe 80GB
	"10de:20f1": true, // A100 PCIe 40GB
	"10de:20f3": true, // A800-SXM4-80GB
	"10de:20b7": true, // A30 PCIe
	"10de:2330": true, // H100 SXM
	"10de:2331": true, // H100 PCIe
	"10de:2337": true, // H100 SXM5 64GB
	"10de:2321": true, // H100 NVL 94GB
	"10de:233a": true, // H800L 94GB
	"10de:2335": true, // H200 SXM
	"10de:233b": true, // H200 NVL
	"10de:2bb5": true, // RTX Pro 6000 Blackwell Server Edition
}

// IsComputeGPU reports whether vendorID:deviceID identifies a headless compute GPU
// that should use x-vga=off in QEMU. Consumer/display GPUs return false.
// Both IDs must be lowercase hex without a "0x" prefix.
func IsComputeGPU(vendorID, deviceID string) bool {
	return computeModels[vendorID+":"+deviceID]
}

// IsMIGCapable reports whether vendorID:deviceID identifies a GPU that supports
// NVIDIA MIG partitioning. Both IDs must be lowercase hex without a "0x" prefix.
func IsMIGCapable(vendorID, deviceID string) bool {
	return migCapableModels[vendorID+":"+deviceID]
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
