//test:in-package — pins instanceFamilyDefs and gpuCountPerType against each
// other. Both are unexported by design: the point of the pin is that a size
// added without a count fails before the type is ever generated.
package instancetypes

import (
	"fmt"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gpuFamilies are every family in instanceFamilyDefs that GenerateGPUTypes can
// emit. Listed here rather than derived so a new GPU family has to be added
// deliberately, with its counts.
var gpuFamilies = []string{
	"g4dn", "g4ad", "g5", "g6", "gr6", "g6e", "g7e",
	"p3", "p3dn", "p4d", "p4de", "p5", "p5e",
	builtinGPUFamily,
}

// wantGPUCounts pins the GPU count of every size of every GPU family. A size
// added without a count, or a count added without a size, fails this test —
// which is the only way a wrong figure surfaces before a customer sizes a
// workload from it. AWS figures are the real EC2 ones; a Spinifex-invented
// size is marked.
var wantGPUCounts = map[string]int{
	"g4dn.xlarge":   1,
	"g4dn.2xlarge":  1,
	"g4dn.4xlarge":  1,
	"g4dn.8xlarge":  1,
	"g4dn.12xlarge": 4,
	"g4dn.16xlarge": 1,

	"g4ad.xlarge":   1,
	"g4ad.2xlarge":  1,
	"g4ad.4xlarge":  1,
	"g4ad.8xlarge":  2,
	"g4ad.16xlarge": 4,

	"g5.xlarge":   1,
	"g5.2xlarge":  1,
	"g5.4xlarge":  1,
	"g5.8xlarge":  1,
	"g5.12xlarge": 4,
	"g5.16xlarge": 1,
	"g5.24xlarge": 4,
	"g5.48xlarge": 8,

	"g6.xlarge":   1,
	"g6.2xlarge":  1,
	"g6.4xlarge":  1,
	"g6.8xlarge":  1,
	"g6.12xlarge": 4,
	"g6.16xlarge": 1,
	"g6.24xlarge": 4,
	"g6.48xlarge": 8,

	"gr6.4xlarge": 1,
	"gr6.8xlarge": 1,

	"g6e.xlarge":   1,
	"g6e.2xlarge":  1,
	"g6e.4xlarge":  1,
	"g6e.8xlarge":  1,
	"g6e.12xlarge": 4,
	"g6e.16xlarge": 1,
	"g6e.24xlarge": 4,
	"g6e.48xlarge": 8,

	// g7e is a Spinifex family, grandfathered and not extended.
	"g7e.2xlarge":  1,
	"g7e.4xlarge":  1,
	"g7e.8xlarge":  1,
	"g7e.12xlarge": 2,

	"p3.2xlarge":  1,
	"p3.8xlarge":  4,
	"p3.16xlarge": 8,

	"p3dn.2xlarge":  1, // Spinifex single-GPU size
	"p3dn.24xlarge": 8,

	"p4d.xlarge":   1, // Spinifex single-GPU size
	"p4d.24xlarge": 8,

	"p4de.xlarge":   1, // Spinifex single-GPU size
	"p4de.24xlarge": 8,

	"p5.4xlarge":  1, // Spinifex single-GPU size
	"p5.48xlarge": 8,

	"p5e.4xlarge":  1, // Spinifex single-GPU size
	"p5e.48xlarge": 8,
}

func init() {
	// The gpu.* family is generated, so its counts are pinned by the shape
	// assertions in TestBuiltinGPUFamily rather than repeated by hand.
	for n := 1; n <= MaxGPUsPerInstance; n++ {
		for _, perGPU := range builtinGPUTiers {
			wantGPUCounts[fmt.Sprintf("%s%dx%dc", builtinGPUPrefix, n, perGPU)] = n
		}
	}
}

func TestGPUCountForType_PinsEveryGPUSize(t *testing.T) {
	defined := make(map[string]bool, len(wantGPUCounts))

	for _, def := range instanceFamilyDefs {
		if !slices.Contains(gpuFamilies, def.name) {
			continue
		}
		for _, size := range def.sizes {
			name := def.name + "." + size.suffix
			defined[name] = true
			want, pinned := wantGPUCounts[name]
			if !assert.True(t, pinned, "%s has no pinned GPU count", name) {
				continue
			}
			assert.Equal(t, want, GPUCountForType(name), "GPU count for %s", name)
		}
	}

	for name := range wantGPUCounts {
		assert.True(t, defined[name], "%s is pinned but no longer defined — removing a type is breaking", name)
	}
}

// Every name in gpuCountPerType must be a type that exists. An entry for a size
// that was never defined silently does nothing.
func TestGPUCountPerType_HasNoOrphanEntries(t *testing.T) {
	for name := range gpuCountPerType {
		_, ok := DefaultVCPUs(name)
		assert.True(t, ok, "gpuCountPerType names %s, which is not a defined instance type", name)
	}
}

func TestBuiltinGPUFamily(t *testing.T) {
	assert.Len(t, builtinGPUSizes, MaxGPUsPerInstance*len(builtinGPUTiers))

	for n := 1; n <= MaxGPUsPerInstance; n++ {
		for _, perGPU := range builtinGPUTiers {
			name := fmt.Sprintf("%s%dx%dc", builtinGPUPrefix, n, perGPU)
			t.Run(name, func(t *testing.T) {
				wantVCPUs := n * perGPU

				vcpus, ok := DefaultVCPUs(name)
				require.True(t, ok, "%s must be a defined type", name)
				assert.Equal(t, wantVCPUs, vcpus)

				memMiB, ok := DefaultMemoryMiB(name)
				require.True(t, ok)
				assert.Equal(t, int64(wantVCPUs*builtinGPUMemPerVCPUGB*1024), memMiB,
					"every tier carries 4 GiB per vCPU")

				assert.Equal(t, n, GPUCountForType(name))
				assert.True(t, IsBuiltinGPUType(name))
			})
		}
	}
}

// The prefix is reserved and self-announcing, following sys. and mig.
func TestIsBuiltinGPUType(t *testing.T) {
	assert.True(t, IsBuiltinGPUType("gpu.8x4c"))
	assert.True(t, IsBuiltinGPUType("gpu.1x2c"))
	assert.False(t, IsBuiltinGPUType("g5.xlarge"))
	assert.False(t, IsBuiltinGPUType("gpu"))
	assert.False(t, IsBuiltinGPUType(""))
}

// gpu.* and mig.* name a shape and a profile, so neither carries a vendor.
func TestGPUVendorForType_BuiltinAndMIGCarryNoVendor(t *testing.T) {
	assert.Empty(t, GPUVendorForType("gpu.8x4c"))
	assert.Empty(t, GPUVendorForType("mig.1g.10gb"))
	assert.Equal(t, "nvidia", GPUVendorForType("g5.xlarge"))
}

// A node's gpu.* types advertise whatever GPU it discovered, whichever AWS
// family that model maps to.
func TestGenerateGPUTypes_BuiltinFamilyReportsTheDiscoveredModel(t *testing.T) {
	rtx3090 := GPUModel{
		VendorID: "10de", DeviceID: "2204", Family: "g5",
		Manufacturer: "NVIDIA", Name: "GeForce RTX 3090", MemoryMiB: 24576,
	}
	models := make([]GPUModel, 8)
	for i := range models {
		models[i] = rtx3090
	}

	types := GenerateGPUTypes(models, "x86_64")

	it, ok := types["gpu.8x4c"]
	require.True(t, ok, "a homogeneous 8-GPU node must offer gpu.8x4c")
	assert.Equal(t, int64(32), *it.VCpuInfo.DefaultVCpus)
	assert.Equal(t, int64(128*1024), *it.MemoryInfo.SizeInMiB)
	require.Len(t, it.GpuInfo.Gpus, 1)
	assert.Equal(t, int64(8), *it.GpuInfo.Gpus[0].Count)
	assert.Equal(t, "GeForce RTX 3090", *it.GpuInfo.Gpus[0].Name)
	assert.Equal(t, "NVIDIA", *it.GpuInfo.Gpus[0].Manufacturer)
	assert.Equal(t, int64(24576), *it.GpuInfo.Gpus[0].MemoryInfo.SizeInMiB)
	assert.Equal(t, int64(8*24576), *it.GpuInfo.TotalGpuMemoryInMiB)

	// All three tiers, counts 1 through 8, with no gaps.
	for n := 1; n <= MaxGPUsPerInstance; n++ {
		for _, perGPU := range builtinGPUTiers {
			name := fmt.Sprintf("%s%dx%dc", builtinGPUPrefix, n, perGPU)
			assert.Contains(t, types, name)
		}
	}
}

// Two devices advertising the same name and VRAM are interchangeable, so a
// mixed-SKU node of one model (H100 SXM and PCIe differ only by PCI ID) still
// offers multi-GPU types.
func TestGenerateGPUTypes_SameNameAndVRAMIsHomogeneous(t *testing.T) {
	types := GenerateGPUTypes([]GPUModel{NVIDIAh100sxm, NVIDIAh100pcie}, "x86_64")
	assert.Contains(t, types, "gpu.2x4c")
	assert.Contains(t, types, "p5.48xlarge")
}

// Handing one instance a mixture of models tends to make NCCL hang rather than
// fail, so a node that resolved more than one offers single-GPU types only.
func TestGenerateGPUTypes_MixedModelsOfferSingleGPUTypesOnly(t *testing.T) {
	types := GenerateGPUTypes([]GPUModel{NVIDIAa10g, NVIDIAl40s}, "x86_64")

	require.NotEmpty(t, types)
	for name, it := range types {
		require.Len(t, it.GpuInfo.Gpus, 1)
		assert.Equal(t, int64(1), *it.GpuInfo.Gpus[0].Count,
			"%s must not be offered on a node with more than one GPU model", name)
		assert.Equal(t, 1, GPUCountForType(name))
	}

	// The single-GPU sizes of both families survive, and so does gpu.1x*.
	assert.Contains(t, types, "g5.xlarge")
	assert.Contains(t, types, "g6e.xlarge")
	assert.Contains(t, types, "gpu.1x4c")
	assert.NotContains(t, types, "gpu.2x4c")
	assert.NotContains(t, types, "g5.48xlarge")
}

// MIG types stay at count 1: EC2 has no multi-slice type, and an accidental
// capability is awkward to withdraw once someone depends on it.
func TestGenerateMIGTypes_CountIsAlwaysOne(t *testing.T) {
	profiles := []MIGProfileSpec{
		{Name: "1g.10gb", MemoryMiB: 10240},
		{Name: "3g.40gb", MemoryMiB: 40960},
		{Name: "7g.80gb", MemoryMiB: 81920},
	}
	types := GenerateMIGTypes(profiles, "x86_64")
	require.Len(t, types, len(profiles))

	for name, it := range types {
		require.Len(t, it.GpuInfo.Gpus, 1)
		assert.Equal(t, int64(1), *it.GpuInfo.Gpus[0].Count, "%s", name)
		assert.Equal(t, 1, GPUCountForType(name), "%s", name)
		assert.Equal(t, *it.GpuInfo.Gpus[0].MemoryInfo.SizeInMiB, *it.GpuInfo.TotalGpuMemoryInMiB,
			"%s: one slice is the whole allocation", name)
	}
}

// A multi-GPU AWS size must report its real GPU count and the aggregate VRAM
// that follows from it.
func TestGenerateGPUTypes_MultiGPUAWSSizes(t *testing.T) {
	models := make([]GPUModel, 8)
	for i := range models {
		models[i] = NVIDIAa10g
	}
	types := GenerateGPUTypes(models, "x86_64")

	it, ok := types["g5.48xlarge"]
	require.True(t, ok)
	assert.Equal(t, int64(192), *it.VCpuInfo.DefaultVCpus)
	assert.Equal(t, int64(768*1024), *it.MemoryInfo.SizeInMiB)
	assert.Equal(t, int64(8), *it.GpuInfo.Gpus[0].Count)
	assert.Equal(t, int64(8*24576), *it.GpuInfo.TotalGpuMemoryInMiB)
	assert.Equal(t, "g5.48xlarge", aws.StringValue(it.InstanceType))
}
