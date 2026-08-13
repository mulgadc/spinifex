package gpu

// modelInfo describes a GPU model: human-readable name, VRAM, and the
// capability flags that drive passthrough and partitioning decisions.
type modelInfo struct {
	name      string
	memoryMiB int64
	// compute marks a headless datacenter GPU with no display output, which must
	// use x-vga=off for QEMU passthrough. Display GPUs default to x-vga=on.
	compute bool
	// mig marks an NVIDIA GPU supporting MIG partitioning; only ever set on
	// compute models. A10, A10G, L4, L40S and T4 are compute but not MIG.
	mig bool
}

// knownModels maps "vendorID:deviceID" to model info.
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
	"10de:2236": {name: "NVIDIA A10", memoryMiB: 23028, compute: true},
	"10de:2237": {name: "NVIDIA A10G", memoryMiB: 23028, compute: true}, // same GA102 silicon/board as A10
	"10de:2235": {name: "NVIDIA A40", memoryMiB: 45634, compute: true},
	"10de:20b0": {name: "NVIDIA A100 SXM4 40GB", memoryMiB: 40960, compute: true, mig: true},
	"10de:20b2": {name: "NVIDIA A100 SXM4 80GB", memoryMiB: 81920, compute: true, mig: true},
	"10de:20b5": {name: "NVIDIA A100 PCIe 80GB", memoryMiB: 81920, compute: true, mig: true},
	"10de:20f1": {name: "NVIDIA A100 PCIe 40GB", memoryMiB: 40960, compute: true, mig: true},
	"10de:20f3": {name: "NVIDIA A800-SXM4-80GB", memoryMiB: 81920, compute: true, mig: true},
	"10de:20b7": {name: "NVIDIA A30 PCIe", memoryMiB: 24576, compute: true, mig: true},
	"10de:25b6": {name: "NVIDIA A2 / A16", memoryMiB: 15356, compute: true}, // shared ID; both SKUs carry 16GB per physical GPU
	"10de:27b8": {name: "NVIDIA L4", memoryMiB: 23034, compute: true},
	"10de:1eb8": {name: "NVIDIA Tesla T4", memoryMiB: 15109, compute: true},
	"10de:2330": {name: "NVIDIA H100 SXM", memoryMiB: 81920, compute: true, mig: true},
	"10de:2331": {name: "NVIDIA H100 PCIe", memoryMiB: 81920, compute: true, mig: true},
	"10de:2337": {name: "NVIDIA H100 SXM5 64GB", memoryMiB: 65536, compute: true, mig: true},
	"10de:2321": {name: "NVIDIA H100 NVL 94GB", memoryMiB: 94208, compute: true, mig: true},
	"10de:233a": {name: "NVIDIA H800L 94GB", memoryMiB: 94208, compute: true, mig: true},
	"10de:2335": {name: "NVIDIA H200 SXM", memoryMiB: 144384, compute: true, mig: true},
	"10de:233b": {name: "NVIDIA H200 NVL", memoryMiB: 144384, compute: true, mig: true},
	"10de:26b5": {name: "NVIDIA L40", memoryMiB: 46068, compute: true},
	"10de:26b9": {name: "NVIDIA L40S", memoryMiB: 46068, compute: true},
	"10de:2bb5": {name: "NVIDIA RTX Pro 6000 Blackwell Server Edition", memoryMiB: 98304, compute: true, mig: true},
	// NVIDIA workstation (display output; not compute, not MIG)
	"10de:2233": {name: "NVIDIA RTX A5500", memoryMiB: 24576},
	"10de:2230": {name: "NVIDIA RTX A6000", memoryMiB: 49140},
	"10de:2231": {name: "NVIDIA RTX A5000", memoryMiB: 24576},
	"10de:24b0": {name: "NVIDIA RTX A4000", memoryMiB: 16376},
	"10de:2531": {name: "NVIDIA RTX A2000", memoryMiB: 6138},
	"10de:2571": {name: "NVIDIA RTX A2000 12GB", memoryMiB: 12288},
	"10de:25b0": {name: "NVIDIA RTX A1000", memoryMiB: 8188}, // hardware-verified on wattle
	"10de:26b1": {name: "NVIDIA RTX 6000 Ada Generation", memoryMiB: 49152},
	"10de:26b2": {name: "NVIDIA RTX 5000 Ada Generation", memoryMiB: 32768},
	"10de:27b2": {name: "NVIDIA RTX 4000 Ada Generation", memoryMiB: 20480},
	// NVIDIA consumer
	"10de:2684": {name: "NVIDIA GeForce RTX 4090", memoryMiB: 24576},
	"10de:2782": {name: "NVIDIA GeForce RTX 4070 Ti", memoryMiB: 12288},
	"10de:2204": {name: "NVIDIA GeForce RTX 3090", memoryMiB: 24576},
	"10de:2203": {name: "NVIDIA GeForce RTX 3090 Ti", memoryMiB: 24576},
	"10de:2208": {name: "NVIDIA GeForce RTX 3080 Ti", memoryMiB: 12288},
	"10de:2206": {name: "NVIDIA GeForce RTX 3080", memoryMiB: 10240},
	"10de:2487": {name: "NVIDIA GeForce RTX 3060", memoryMiB: 12288},
	"10de:2486": {name: "NVIDIA GeForce RTX 3060 Ti", memoryMiB: 8192},
	"10de:2484": {name: "NVIDIA GeForce RTX 3070", memoryMiB: 8192},
	"10de:1e04": {name: "NVIDIA GeForce RTX 2080 Ti", memoryMiB: 11264},
	// AMD datacenter (compute; AMD has no MIG equivalent)
	"1002:75a0": {name: "AMD Instinct MI350X", memoryMiB: 294896, compute: true}, // absent from local pci.ids snapshot; left unverified per plan
	"1002:74a1": {name: "AMD Instinct MI300X", memoryMiB: 196608, compute: true},
	"1002:74a0": {name: "AMD Instinct MI300A", memoryMiB: 131072, compute: true},
	"1002:740c": {name: "AMD Instinct MI250X", memoryMiB: 65536, compute: true}, // per-GCD VRAM; card exposes 2 PCI devices, 64GB each
	"1002:740f": {name: "AMD Instinct MI210", memoryMiB: 65536, compute: true},
	"1002:738c": {name: "AMD Instinct MI100", memoryMiB: 32768, compute: true},
	// AMD workstation (display output; not compute)
	"1002:7448": {name: "AMD Radeon Pro W7900", memoryMiB: 49152},
	"1002:745e": {name: "AMD Radeon Pro W7800", memoryMiB: 32768},
	// AMD consumer
	"1002:744c": {name: "AMD Radeon RX 7900 XT/XTX/GRE/7900M", memoryMiB: 16384}, // shared ID spans 16/16/20/24GB SKUs; smallest recorded
	"1002:73bf": {name: "AMD Radeon RX 6900 XT", memoryMiB: 16384},
}

// IsComputeGPU reports whether vendorID:deviceID identifies a headless compute GPU
// that should use x-vga=off in QEMU. Consumer/display GPUs return false.
// Both IDs must be lowercase hex without a "0x" prefix.
func IsComputeGPU(vendorID, deviceID string) bool {
	return knownModels[vendorID+":"+deviceID].compute
}

// IsMIGCapable reports whether vendorID:deviceID identifies a GPU that supports
// NVIDIA MIG partitioning. Both IDs must be lowercase hex without a "0x" prefix.
func IsMIGCapable(vendorID, deviceID string) bool {
	return knownModels[vendorID+":"+deviceID].mig
}

// lookupModel returns the model name and VRAM for a known vendorID:deviceID pair.
// Both IDs are expected as lowercase hex without a "0x" prefix.
// Returns zero values if the pair is not in the table.
func lookupModel(vendorID, deviceID string) (name string, memoryMiB int64) {
	info := knownModels[vendorID+":"+deviceID]
	return info.name, info.memoryMiB
}
