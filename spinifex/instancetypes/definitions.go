package instancetypes

import "slices"

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
	NVIDIAh200sxm   = GPUModel{"10de", "2335", "p5e", "NVIDIA", "H200", 144384} // SXM5 141 GiB

	// AMD GPU models
	AMDradeonV520 = GPUModel{"1002", "7362", "g4ad", "AMD", "Radeon Pro V520", 8192}
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
	AMDradeonV520,
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

// Size tables for each instance category

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
	{"micro", 1, 0.125}, // 1 vCPU, 128 MB
}

// instanceFamilyDefs defines all supported instance families with their vendor and sizes.
//
// We support the core families across burstable, general purpose, compute optimized,
// and memory optimized categories. The following AWS family categories are intentionally
// excluded because they require specialized hardware not available on standard bare-metal hosts:
//
//   - Local disk variants (d/n suffixes): c5d, c5ad, c5n, m5d, m5ad, m5n, m5dn, m5zn, r5d, r5ad,
//     r5n, r5dn, r5b, c6gd, c6gn, c6id, c6in, m6gd, m6id, m6idn, m6in, r6gd, r6id, r6idn, r6in,
//     c7gd, c7gn, c7i-flex, m7gd, m7i-flex, r7gd, r7iz, c8gd, c8gn, c8i-flex, m8gd, m8i-flex,
//     r8gd, r8gn, r8gb, r8i-flex — require NVMe instance storage or enhanced networking
//   - Multi-GPU only: g4dn 12xlarge, p3 8/16xlarge, p3dn 24xlarge, p4d/p4de 24xlarge, p5/p5e 48xlarge
//   - AWS-proprietary accelerators: inf1, inf2, trn1, trn2, dl1, dl2q — Inferentia/Trainium chips, AWS-only hardware
//   - Unsupported GPU: g2, g6f, gr6f, p2, p6 — too old (g2/p2), future/unannounced (p6), or insufficient hardware availability (g6f/gr6f)
//   - Storage optimized: d2, d3, d3en, h1, i2-i8g, i7ie, i8ge, im4gn, is4gen — require dense HDD/NVMe
//   - FPGA: f1, f2 — require FPGA hardware
//   - High memory: u-*, u7i-*, x1, x1e, x2gd, x2idn, x2iedn, x2iezn, x8g — require TB-scale memory
//   - High frequency: z1d — specialized high clock-speed instances
//   - (unsupported) Dedicated host: mac*, hpc* — require macOS/Apple hardware or HPC interconnects
//   - (unsupported) Video: vt1 — requires video transcoding hardware
//   - Legacy (pre-gen4): a1, c1, c3, cc1, cc2, cg1, cr1, hi1, hs1, m1, m2, m3, r3, t1
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

	// GPU Accelerated — not emitted by generateForGeneration; used by generateGPUTypes.
	{name: "g4dn", sizes: g4dnSizes, currentGen: true},
	{name: "g4ad", sizes: g4adSizes, currentGen: true},
	{name: "g5", sizes: g5Sizes, currentGen: true},
	{name: "g6", sizes: g6Sizes, currentGen: true},
	{name: "gr6", sizes: gr6Sizes, currentGen: true}, // same L4 GPU as g6; requires operator config (PCI ID indistinguishable)
	{name: "g6e", sizes: g6eSizes, currentGen: true},
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
