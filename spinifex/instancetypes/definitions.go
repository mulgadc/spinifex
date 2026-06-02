package instancetypes

import (
	"slices"
	"strings"
)

// GPUModel describes a GPU device model that maps to a specific GPU instance family.
type GPUModel struct {
	VendorID     string // PCI vendor ID, lowercase hex, e.g. "10de"
	DeviceID     string // PCI device ID, lowercase hex, e.g. "2236"
	Family       string // instance family prefix, e.g. "g5"
	Manufacturer string // e.g. "NVIDIA"
	Name         string // e.g. "A10G"
	MemoryMiB    int64
}

var (
	// NVIDIA GPU models
	NVIDIAa10g      = GPUModel{"10de", "2236", "g5", "NVIDIA", "A10G", 24576}
	NVIDIAt4        = GPUModel{"10de", "1eb8", "g4dn", "NVIDIA", "T4", 16384}
	NVIDIAl4        = GPUModel{"10de", "27b8", "g6", "NVIDIA", "L4", 24576} // gr6 uses identical hardware; requires operator config
	NVIDIAl40s      = GPUModel{"10de", "26b9", "g6e", "NVIDIA", "L40S", 49152}
	NVIDIAv100sxm16 = GPUModel{"10de", "1db1", "p3", "NVIDIA", "V100", 16384}   // SXM2 16 GiB
	NVIDIAv100sxm32 = GPUModel{"10de", "1db3", "p3dn", "NVIDIA", "V100", 32768} // SXM2 32 GiB
	NVIDIAv100pcie  = GPUModel{"10de", "1dba", "p3", "NVIDIA", "V100", 16384}   // PCIe 16 GiB
	NVIDIAa100sxm40 = GPUModel{"10de", "20b0", "p4d", "NVIDIA", "A100", 40960}  // SXM4 40 GiB
	NVIDIAa100sxm80 = GPUModel{"10de", "20b5", "p4de", "NVIDIA", "A100", 81920} // SXM4 80 GiB
	NVIDIAh100sxm   = GPUModel{"10de", "2330", "p5", "NVIDIA", "H100", 81920}   // SXM5 80 GiB
	NVIDIAh100pcie  = GPUModel{"10de", "2331", "p5", "NVIDIA", "H100", 81920}   // PCIe 80 GiB
	NVIDIAh200sxm              = GPUModel{"10de", "2335", "p5e", "NVIDIA", "H200", 144384}                                   // SXM5 141 GiB
	NVIDIArtxPro6000BlackwellSE = GPUModel{"10de", "2bb5", "g7e", "NVIDIA", "RTX Pro 6000 Blackwell Server Edition", 98304} // 96 GiB GDDR7

	// AMD GPU models
	AMDradeonV520 = GPUModel{"1002", "7362", "g4ad", "AMD", "Radeon Pro V520", 8192}
	AMDmi350x     = GPUModel{"1002", "75a0", "g7e", "AMD", "Instinct MI350X", 294896}
)

var knownGPUModels = []GPUModel{
	NVIDIAa10g,
	NVIDIAt4,
	NVIDIAl4,
	NVIDIAl40s,
	NVIDIAv100sxm16,
	NVIDIAv100sxm32,
	NVIDIAv100pcie,
	NVIDIAa100sxm40,
	NVIDIAa100sxm80,
	NVIDIAh100sxm,
	NVIDIAh100pcie,
	NVIDIAh200sxm,
	NVIDIArtxPro6000BlackwellSE,
	AMDradeonV520,
	AMDmi350x,
}

// GPUModelForVendorDevice returns the GPUModel for a PCI vendor/device ID pair,
// or nil if the device is not a recognized GPU model.
func GPUModelForVendorDevice(vendorID, deviceID string) *GPUModel {
	for i := range knownGPUModels {
		if knownGPUModels[i].VendorID == vendorID && knownGPUModels[i].DeviceID == deviceID {
			return &knownGPUModels[i]
		}
	}
	return nil
}

// cpuGeneration represents a specific CPU microarchitecture generation
// and the AWS instance families it maps to.
type cpuGeneration struct {
	name     string   // e.g. "Intel Ice Lake", "AMD Genoa"
	families []string // e.g. ["t3", "c6i", "m6i", "r6i"]
}

// vendorSiblingFamily maps an x86_64 family to its cross-vendor counterpart
// (Intel ↔ AMD) within the same generation tier. Siblings share the same vCPU/memory
// ratios, so a mixed Intel+AMD cluster can serve either vendor family.
var vendorSiblingFamily = map[string]string{
	"t3": "t3a", "t3a": "t3",
	"c5": "c5a", "c5a": "c5",
	"m5": "m5a", "m5a": "m5",
	"r5": "r5a", "r5a": "r5",
	"c6i": "c6a", "c6a": "c6i",
	"m6i": "m6a", "m6a": "m6i",
	"r6i": "r6a", "r6a": "r6i",
	"c7i": "c7a", "c7a": "c7i",
	"m7i": "m7a", "m7a": "m7i",
	"r7i": "r7a", "r7a": "r7i",
	"c8i": "c8a", "c8a": "c8i",
	"m8i": "m8a", "m8a": "m8i",
	"r8i": "r8a", "r8a": "r8i",
}

var (
	// Intel generations
	genIntelBroadwell      = cpuGeneration{"Intel Broadwell", []string{"t2", "c4", "m4", "r4"}}
	genIntelSkylake        = cpuGeneration{"Intel Skylake/Cascade Lake", []string{"t3", "c5", "m5", "r5"}}
	genIntelIceLake        = cpuGeneration{"Intel Ice Lake", []string{"t3", "c6i", "m6i", "r6i"}}
	genIntelSapphireRapids = cpuGeneration{"Intel Sapphire Rapids", []string{"t3", "c7i", "m7i", "r7i"}}
	genIntelGraniteRapids  = cpuGeneration{"Intel Granite Rapids", []string{"t3", "c8i", "m8i", "r8i"}}

	// AMD generations
	genAMDZen  = cpuGeneration{"AMD Zen/Zen2 (Naples/Rome)", []string{"t3a", "c5a", "m5a", "r5a"}}
	genAMDZen3 = cpuGeneration{"AMD Zen 3 (Milan)", []string{"t3a", "c6a", "m6a", "r6a"}}
	genAMDZen4 = cpuGeneration{"AMD Zen 4 (Genoa)", []string{"t3a", "c7a", "m7a", "r7a"}}
	genAMDZen5 = cpuGeneration{"AMD Zen 5 (Turin)", []string{"t3a", "c8a", "m8a", "r8a"}}

	// ARM generations
	genARMNeoverseN1 = cpuGeneration{"ARM Neoverse N1 (Graviton2)", []string{"t4g", "c6g", "m6g", "r6g"}}
	genARMNeoverseV1 = cpuGeneration{"ARM Neoverse V1 (Graviton3)", []string{"t4g", "c7g", "m7g", "r7g"}}
	genARMNeoverseV2 = cpuGeneration{"ARM Neoverse V2 (Graviton4)", []string{"t4g", "c8g", "m8g", "r8g"}}

	// Unknown/fallback — expose only burstable family
	genUnknownIntel = cpuGeneration{"Unknown Intel", []string{"t3"}}
	genUnknownAMD   = cpuGeneration{"Unknown AMD", []string{"t3a"}}
	genUnknownARM   = cpuGeneration{"Unknown ARM", []string{"t4g"}}
	genUnknown      = cpuGeneration{"Unknown", []string{"t3"}}
)

type instanceSize struct {
	suffix   string
	vcpus    int
	memoryGB float64
}

type instanceFamilyDef struct {
	name       string
	sizes      []instanceSize
	currentGen bool
}

var burstableSizes = []instanceSize{
	{"nano", 2, 0.5},
	{"micro", 2, 1},
	{"small", 2, 2},
	{"medium", 2, 4},
	{"large", 2, 8},
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
}

var gpSizes = []instanceSize{
	{"large", 2, 8},
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
	{"4xlarge", 16, 64},
	{"8xlarge", 32, 128},
	{"12xlarge", 48, 192},
	{"16xlarge", 64, 256},
	{"24xlarge", 96, 384},
}

// gpSizesSmall is gpSizes without 12xlarge and 24xlarge (older/ARM families).
var gpSizesSmall = slices.Clone(gpSizes[:6])

var computeSizes = []instanceSize{
	{"large", 2, 4},
	{"xlarge", 4, 8},
	{"2xlarge", 8, 16},
	{"4xlarge", 16, 32},
	{"8xlarge", 32, 64},
	{"12xlarge", 48, 96},
	{"16xlarge", 64, 128},
	{"24xlarge", 96, 192},
}

// computeSizesSmall is computeSizes without 12xlarge and 24xlarge (older/ARM families).
var computeSizesSmall = slices.Clone(computeSizes[:6])

var memorySizes = []instanceSize{
	{"large", 2, 16},
	{"xlarge", 4, 32},
	{"2xlarge", 8, 64},
	{"4xlarge", 16, 128},
	{"8xlarge", 32, 256},
	{"12xlarge", 48, 384},
	{"16xlarge", 64, 512},
	{"24xlarge", 96, 768},
}

// memorySizesSmall is memorySizes without 12xlarge and 24xlarge (older/ARM families).
var memorySizesSmall = slices.Clone(memorySizes[:6])

// g4dnSizes are the single-GPU G4dn instance sizes (1x NVIDIA T4 each).
var g4dnSizes = []instanceSize{
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
	{"4xlarge", 16, 64},
	{"8xlarge", 32, 128},
	{"16xlarge", 64, 256},
}

// g4adSizes are the single-GPU G4ad instance sizes (1x AMD Radeon Pro V520 each).
var g4adSizes = []instanceSize{
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
	{"4xlarge", 16, 64},
}

// g5Sizes are the single-GPU G5 instance sizes (1x NVIDIA A10G each).
var g5Sizes = []instanceSize{
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
	{"4xlarge", 16, 64},
	{"8xlarge", 32, 128},
	{"16xlarge", 64, 256},
}

// g6Sizes are the single-GPU G6 instance sizes (1x NVIDIA L4 each).
var g6Sizes = []instanceSize{
	{"xlarge", 4, 16},
	{"2xlarge", 8, 32},
	{"4xlarge", 16, 64},
	{"8xlarge", 32, 128},
	{"16xlarge", 64, 256},
}

// gr6Sizes are the single-GPU Gr6 instance sizes (1x NVIDIA L4, memory-optimized).
var gr6Sizes = []instanceSize{
	{"4xlarge", 16, 128},
	{"8xlarge", 32, 256},
}

// g7eSizes are the G7e instance sizes (NVIDIA RTX Pro 6000 Blackwell Server Edition / AMD Instinct MI350X).
// 12xlarge carries 2 GPUs; sizes above that are excluded (require 4+ GPUs).
var g7eSizes = []instanceSize{
	{"2xlarge", 8, 64},
	{"4xlarge", 16, 128},
	{"8xlarge", 32, 256},
	{"12xlarge", 48, 512},
}

// gpuCountPerType overrides the default of 1 GPU for multi-GPU instance sizes.
var gpuCountPerType = map[string]int{
	"g7e.12xlarge": 2,
}

// GPUCountForType returns the number of GPUs required by the given instance type.
// Returns 1 for all single-GPU and non-GPU types.
func GPUCountForType(instanceType string) int {
	if n, ok := gpuCountPerType[instanceType]; ok {
		return n
	}
	return 1
}

// g6eSizes are the single-GPU G6e instance sizes (1x NVIDIA L40S each).
var g6eSizes = []instanceSize{
	{"xlarge", 4, 32},
	{"2xlarge", 8, 64},
	{"4xlarge", 16, 128},
	{"8xlarge", 32, 256},
	{"16xlarge", 64, 512},
}

// p3Sizes are the single-GPU P3 instance sizes (1x NVIDIA V100 16 GiB each).
// P3 8xlarge and larger have multiple GPUs and are excluded.
var p3Sizes = []instanceSize{
	{"2xlarge", 8, 61},
}

// p3dnSizes are the single-GPU P3dn instance sizes (1x NVIDIA V100 32 GiB each).
// P3dn 24xlarge has 8 GPUs and is excluded.
var p3dnSizes = []instanceSize{
	{"2xlarge", 8, 61},
}

// p4dSizes are the single-GPU P4d instance sizes (1x NVIDIA A100 SXM4 40 GiB each).
// P4d 24xlarge has 8 GPUs and is excluded.
var p4dSizes = []instanceSize{
	{"xlarge", 4, 32},
}

// p4deSizes are the single-GPU P4de instance sizes (1x NVIDIA A100 SXM4 80 GiB each).
// P4de 24xlarge has 8 GPUs and is excluded.
var p4deSizes = []instanceSize{
	{"xlarge", 4, 32},
}

// p5Sizes are the single-GPU P5 instance sizes (1x NVIDIA H100 SXM5 80 GiB each).
// P5 48xlarge has 8 GPUs and is excluded.
var p5Sizes = []instanceSize{
	{"4xlarge", 16, 256},
}

// p5eSizes are the single-GPU P5e instance sizes (1x NVIDIA H200 SXM5 141 GiB each).
// P5e 48xlarge has 8 GPUs and is excluded.
var p5eSizes = []instanceSize{
	{"4xlarge", 16, 256},
}

// systemSizes defines internal-only instance types for system VMs (LB, NAT GW, etc.).
// These are registered in the type map for allocation but excluded from DescribeInstanceTypes.
var systemSizes = []instanceSize{
	{"micro", 1, 0.125}, // 1 vCPU, 128 MB — ELBv2 LB microVMs
	{"medium", 2, 4},    // 2 vCPU, 4 GB — EKS k3s control-plane VMs
}

// instanceFamilyDefs lists all supported instance families. Excluded families require
// hardware not available on bare-metal: local NVMe, Inferentia/Trainium, FPGAs, TB-scale
// memory, multi-GPU-only sizes, or macOS/HPC interconnects.
var instanceFamilyDefs = []instanceFamilyDef{
	// Burstable
	{name: "t2", sizes: burstableSizes, currentGen: false},
	{name: "t3", sizes: burstableSizes, currentGen: true},
	{name: "t3a", sizes: burstableSizes, currentGen: true},
	{name: "t4g", sizes: burstableSizes, currentGen: true},

	// General Purpose (1:4 vCPU:memory)
	{name: "m4", sizes: gpSizesSmall, currentGen: false},
	{name: "m5", sizes: gpSizes, currentGen: true},
	{name: "m5a", sizes: gpSizes, currentGen: true},
	{name: "m6i", sizes: gpSizes, currentGen: true},
	{name: "m6a", sizes: gpSizes, currentGen: true},
	{name: "m6g", sizes: gpSizesSmall, currentGen: true},
	{name: "m7i", sizes: gpSizes, currentGen: true},
	{name: "m7a", sizes: gpSizes, currentGen: true},
	{name: "m7g", sizes: gpSizesSmall, currentGen: true},
	{name: "m8i", sizes: gpSizes, currentGen: true},
	{name: "m8a", sizes: gpSizes, currentGen: true},
	{name: "m8g", sizes: gpSizesSmall, currentGen: true},

	// Compute Optimized (1:2 vCPU:memory)
	{name: "c4", sizes: computeSizesSmall, currentGen: false},
	{name: "c5", sizes: computeSizes, currentGen: true},
	{name: "c5a", sizes: computeSizes, currentGen: true},
	{name: "c6i", sizes: computeSizes, currentGen: true},
	{name: "c6a", sizes: computeSizes, currentGen: true},
	{name: "c6g", sizes: computeSizesSmall, currentGen: true},
	{name: "c7i", sizes: computeSizes, currentGen: true},
	{name: "c7a", sizes: computeSizes, currentGen: true},
	{name: "c7g", sizes: computeSizesSmall, currentGen: true},
	{name: "c8i", sizes: computeSizes, currentGen: true},
	{name: "c8a", sizes: computeSizes, currentGen: true},
	{name: "c8g", sizes: computeSizesSmall, currentGen: true},

	// System (internal-only, not exposed via DescribeInstanceTypes)
	{name: "sys", sizes: systemSizes, currentGen: true},

	// GPU Accelerated — not emitted by generateForGeneration; used by GenerateGPUTypes.
	{name: "g4dn", sizes: g4dnSizes, currentGen: true},
	{name: "g4ad", sizes: g4adSizes, currentGen: true},
	{name: "g5", sizes: g5Sizes, currentGen: true},
	{name: "g6", sizes: g6Sizes, currentGen: true},
	{name: "gr6", sizes: gr6Sizes, currentGen: true}, // same L4 GPU as g6; requires operator config (PCI ID indistinguishable)
	{name: "g6e", sizes: g6eSizes, currentGen: true},
	{name: "g7e", sizes: g7eSizes, currentGen: true},
	{name: "p3", sizes: p3Sizes, currentGen: false},
	{name: "p3dn", sizes: p3dnSizes, currentGen: false},
	{name: "p4d", sizes: p4dSizes, currentGen: true},
	{name: "p4de", sizes: p4deSizes, currentGen: true},
	{name: "p5", sizes: p5Sizes, currentGen: true},
	{name: "p5e", sizes: p5eSizes, currentGen: true},

	// Memory Optimized (1:8 vCPU:memory)
	{name: "r4", sizes: memorySizesSmall, currentGen: false},
	{name: "r5", sizes: memorySizes, currentGen: true},
	{name: "r5a", sizes: memorySizes, currentGen: true},
	{name: "r6i", sizes: memorySizes, currentGen: true},
	{name: "r6a", sizes: memorySizes, currentGen: true},
	{name: "r6g", sizes: memorySizesSmall, currentGen: true},
	{name: "r7i", sizes: memorySizes, currentGen: true},
	{name: "r7a", sizes: memorySizes, currentGen: true},
	{name: "r7g", sizes: memorySizesSmall, currentGen: true},
	{name: "r8i", sizes: memorySizes, currentGen: true},
	{name: "r8a", sizes: memorySizes, currentGen: true},
	{name: "r8g", sizes: memorySizesSmall, currentGen: true},
}

// migHostResources maps a MIG profile name to the host vCPU count and memory
// that EC2 instances of that type receive. MIG only partitions GPU resources;
// host CPU/RAM allocation is a policy decision with no NVIDIA-defined mapping.
//
// Sizing rule: 4 vCPUs and 32 GiB host RAM per compute slice (the leading digit
// in the profile name, e.g. "3g.40gb" = 3 slices → 12 vCPUs / 96 GiB).
// This table covers all profiles defined by NVIDIA for A100, H100, H100 NVL,
// H200, A30, and RTX Pro 6000 Blackwell Server Edition.
var migHostResources = map[string]struct {
	vcpus    int
	memoryGB float64
}{
	// A100 40 GiB (7-way MIG)
	"1g.5gb":  {4, 32},
	"2g.10gb": {8, 64},
	"3g.20gb": {12, 96},
	"4g.20gb": {16, 128},
	"7g.40gb": {28, 224},
	// A100 80 GiB / H100 SXM 80 GiB (7-way MIG)
	"1g.10gb": {4, 32},
	"2g.20gb": {8, 64},
	"3g.40gb": {12, 96},
	"4g.40gb": {16, 128},
	"7g.80gb": {28, 224},
	// H100 NVL 94 GiB (7-way MIG)
	"1g.12gb": {4, 32},
	"2g.24gb": {8, 64},
	"3g.47gb": {12, 96},
	"4g.47gb": {16, 128},
	"7g.94gb": {28, 224},
	// H200 SXM 141 GiB (7-way MIG)
	"1g.18gb":  {4, 32},
	"2g.35gb":  {8, 64},
	"4g.71gb":  {16, 128},
	"7g.141gb": {28, 224},
	// A30 24 GiB (4-way MIG)
	"1g.6gb": {4, 32},
	"2g.12gb": {8, 64},
	"4g.24gb": {16, 128},
	// RTX Pro 6000 Blackwell Server Edition 96 GiB (4-way MIG)
	"1g.24gb": {4, 32},
	"2g.48gb": {8, 64},
	"4g.96gb": {16, 128},
}

// migHostDefault is used for unrecognised profile names. Mirrors a single
// 1-slice allocation: conservative enough for the smallest known profiles.
var migHostDefault = struct {
	vcpus    int
	memoryGB float64
}{4, 32}

// MIGHostResources returns the host vCPU count and memory in MiB to assign to
// an EC2 instance of the given MIG profile name (e.g. "1g.10gb"). The profile
// name is matched after stripping any hardware-specific suffixes (+gfx, -me,
// +me) that nvidia-smi appends on some architectures. Returns a 4-vCPU /
// 32-GiB default for profiles not present in the table.
func MIGHostResources(profileName string) (vcpus, memMiB int64) {
	// Strip Blackwell/Hopper suffixes that nvidia-smi appends.
	clean := profileName
	for _, suffix := range []string{"+gfx", "+me", "-me"} {
		clean = strings.TrimSuffix(clean, suffix)
	}
	s, ok := migHostResources[clean]
	if !ok {
		s = migHostDefault
	}
	return int64(s.vcpus), int64(s.memoryGB * 1024)
}
