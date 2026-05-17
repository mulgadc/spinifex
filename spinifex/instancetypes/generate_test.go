package instancetypes

import (
	"maps"
	"strings"
	"testing"

	cpuid "github.com/klauspost/cpuid/v2"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasFamily(types map[string]*ec2.InstanceTypeInfo, prefix string) bool {
	for name := range types {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func countFamily(types map[string]*ec2.InstanceTypeInfo, prefix string) int {
	count := 0
	for name := range types {
		if strings.HasPrefix(name, prefix) {
			count++
		}
	}
	return count
}

func TestIsSystemType(t *testing.T) {
	assert.True(t, IsSystemType("sys.micro"))
	assert.True(t, IsSystemType("sys.small"))
	assert.False(t, IsSystemType("t3.micro"))
	assert.False(t, IsSystemType("m5.large"))
	assert.False(t, IsSystemType("system.large"))
}

func TestGenerateSystemTypes(t *testing.T) {
	types := generateSystemTypes("x86_64")
	require.Len(t, types, 1, "should have exactly 1 system type (sys.micro)")

	sysMicro, ok := types["sys.micro"]
	require.True(t, ok, "sys.micro must exist")
	assert.Equal(t, int64(1), *sysMicro.VCpuInfo.DefaultVCpus, "sys.micro should have 1 vCPU")
	assert.Equal(t, int64(128), *sysMicro.MemoryInfo.SizeInMiB, "sys.micro should have 128 MiB")
	assert.False(t, *sysMicro.BurstablePerformanceSupported, "sys.micro should not be burstable")
}

func TestDetectAndGenerate_IncludesSystemTypes(t *testing.T) {
	// Use any generation — system types should always be present
	types := generateForGeneration(genIntelSkylake, "x86_64")
	_, hasSys := types["sys.micro"]
	assert.False(t, hasSys, "generateForGeneration alone should not include system types")

	// DetectAndGenerate merges system types in
	// We can't easily call DetectAndGenerate in tests (needs real CPU),
	// so verify generateSystemTypes output merges correctly.
	maps.Copy(types, generateSystemTypes("x86_64"))
	_, hasSys = types["sys.micro"]
	assert.True(t, hasSys, "merged map should include sys.micro")
}

func TestGenerateInstanceTypes_IntelIceLake(t *testing.T) {
	types := generateForGeneration(genIntelIceLake, "x86_64")
	// t3(7) + c6i(8) + m6i(8) + r6i(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3.", "c6i.", "m6i.", "r6i."} {
		assert.True(t, hasFamily(types, prefix), "expected Ice Lake types to include %s family", prefix)
	}

	// Verify other generation families are NOT present
	for name := range types {
		assert.False(t, strings.HasPrefix(name, "t3a."), "Ice Lake should not have t3a: %s", name)
		assert.False(t, strings.HasPrefix(name, "c5."), "Ice Lake should not have c5: %s", name)
		assert.False(t, strings.HasPrefix(name, "c7i."), "Ice Lake should not have c7i: %s", name)
	}
}

func TestGenerateInstanceTypes_IntelBroadwell(t *testing.T) {
	types := generateForGeneration(genIntelBroadwell, "x86_64")
	// t2(7) + c4(6) + m4(6) + r4(6) = 25
	assert.Len(t, types, 25)

	for _, prefix := range []string{"t2.", "c4.", "m4.", "r4."} {
		assert.True(t, hasFamily(types, prefix), "expected Broadwell types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_IntelSkylake(t *testing.T) {
	types := generateForGeneration(genIntelSkylake, "x86_64")
	// t3(7) + c5(8) + m5(8) + r5(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3.", "c5.", "m5.", "r5."} {
		assert.True(t, hasFamily(types, prefix), "expected Skylake types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_IntelSapphireRapids(t *testing.T) {
	types := generateForGeneration(genIntelSapphireRapids, "x86_64")
	// t3(7) + c7i(8) + m7i(8) + r7i(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3.", "c7i.", "m7i.", "r7i."} {
		assert.True(t, hasFamily(types, prefix), "expected Sapphire Rapids types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_IntelGraniteRapids(t *testing.T) {
	types := generateForGeneration(genIntelGraniteRapids, "x86_64")
	// t3(7) + c8i(8) + m8i(8) + r8i(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3.", "c8i.", "m8i.", "r8i."} {
		assert.True(t, hasFamily(types, prefix), "expected Granite Rapids types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_AMDZen(t *testing.T) {
	types := generateForGeneration(genAMDZen, "x86_64")
	// t3a(7) + c5a(8) + m5a(8) + r5a(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3a.", "c5a.", "m5a.", "r5a."} {
		assert.True(t, hasFamily(types, prefix), "expected Zen types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_AMDZen4(t *testing.T) {
	types := generateForGeneration(genAMDZen4, "x86_64")
	// t3a(7) + c7a(8) + m7a(8) + r7a(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3a.", "c7a.", "m7a.", "r7a."} {
		assert.True(t, hasFamily(types, prefix), "expected Zen 4 types to include %s family", prefix)
	}

	// Verify older AMD families are NOT present
	for name := range types {
		assert.False(t, strings.HasPrefix(name, "c5a."), "Zen4 should not have c5a: %s", name)
		assert.False(t, strings.HasPrefix(name, "c6a."), "Zen4 should not have c6a: %s", name)
	}
}

func TestGenerateInstanceTypes_AMDZen3(t *testing.T) {
	types := generateForGeneration(genAMDZen3, "x86_64")
	// t3a(7) + c6a(8) + m6a(8) + r6a(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3a.", "c6a.", "m6a.", "r6a."} {
		assert.True(t, hasFamily(types, prefix), "expected Zen 3 types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_AMDZen5(t *testing.T) {
	types := generateForGeneration(genAMDZen5, "x86_64")
	// t3a(7) + c8a(8) + m8a(8) + r8a(8) = 31
	assert.Len(t, types, 31)

	for _, prefix := range []string{"t3a.", "c8a.", "m8a.", "r8a."} {
		assert.True(t, hasFamily(types, prefix), "expected Zen 5 types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_ARMV1(t *testing.T) {
	types := generateForGeneration(genARMNeoverseV1, "arm64")
	// t4g(7) + c7g(6) + m7g(6) + r7g(6) = 25
	assert.Len(t, types, 25)

	for _, prefix := range []string{"t4g.", "c7g.", "m7g.", "r7g."} {
		assert.True(t, hasFamily(types, prefix), "expected V1 types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_ARMN1(t *testing.T) {
	types := generateForGeneration(genARMNeoverseN1, "arm64")
	// t4g(7) + c6g(6) + m6g(6) + r6g(6) = 25
	assert.Len(t, types, 25)

	for _, prefix := range []string{"t4g.", "c6g.", "m6g.", "r6g."} {
		assert.True(t, hasFamily(types, prefix), "expected N1 types to include %s family", prefix)
	}

	// Verify Intel/AMD families are NOT present
	for name := range types {
		assert.False(t, strings.HasPrefix(name, "t3."), "ARM should not have t3: %s", name)
		assert.False(t, strings.HasPrefix(name, "t3a."), "ARM should not have t3a: %s", name)
	}
}

func TestGenerateInstanceTypes_ARMV2(t *testing.T) {
	types := generateForGeneration(genARMNeoverseV2, "arm64")
	// t4g(7) + c8g(6) + m8g(6) + r8g(6) = 25
	assert.Len(t, types, 25)

	for _, prefix := range []string{"t4g.", "c8g.", "m8g.", "r8g."} {
		assert.True(t, hasFamily(types, prefix), "expected V2 types to include %s family", prefix)
	}
}

func TestGenerateInstanceTypes_UnknownFallback(t *testing.T) {
	types := generateForGeneration(genUnknownIntel, "x86_64")
	// Unknown Intel: t3 only = 7 types
	assert.Len(t, types, 7)
	assert.True(t, hasFamily(types, "t3."), "unknown Intel should have t3")

	types = generateForGeneration(genUnknownAMD, "x86_64")
	assert.Len(t, types, 7)
	assert.True(t, hasFamily(types, "t3a."), "unknown AMD should have t3a")

	types = generateForGeneration(genUnknownARM, "arm64")
	assert.Len(t, types, 7)
	assert.True(t, hasFamily(types, "t4g."), "unknown ARM should have t4g")

	types = generateForGeneration(genUnknown, "x86_64")
	assert.Len(t, types, 7)
	assert.True(t, hasFamily(types, "t3."), "completely unknown should have t3")
}

func TestGenerateInstanceTypes_VerifyBurstableSizes(t *testing.T) {
	types := generateForGeneration(genIntelSkylake, "x86_64")

	expected := map[string]struct {
		vcpus int64
		memMB int64
	}{
		"t3.nano":    {2, 512},
		"t3.micro":   {2, 1024},
		"t3.small":   {2, 2048},
		"t3.medium":  {2, 4096},
		"t3.large":   {2, 8192},
		"t3.xlarge":  {4, 16384},
		"t3.2xlarge": {8, 32768},
	}

	for name, exp := range expected {
		it, ok := types[name]
		require.True(t, ok, "missing instance type %s", name)
		assert.Equal(t, exp.vcpus, *it.VCpuInfo.DefaultVCpus, "%s vCPUs", name)
		assert.Equal(t, exp.memMB, *it.MemoryInfo.SizeInMiB, "%s memory", name)
	}
}

func TestGenerateInstanceTypes_ComputeRatio(t *testing.T) {
	// Skylake for c5
	skylakeTypes := generateForGeneration(genIntelSkylake, "x86_64")
	expectedSkylake := map[string]struct {
		vcpus int64
		memMB int64
	}{
		"c5.large":   {2, 4096},
		"c5.xlarge":  {4, 8192},
		"c5.2xlarge": {8, 16384},
	}

	for name, exp := range expectedSkylake {
		it, ok := skylakeTypes[name]
		require.True(t, ok, "missing instance type %s", name)
		assert.Equal(t, exp.vcpus, *it.VCpuInfo.DefaultVCpus, "%s vCPUs", name)
		assert.Equal(t, exp.memMB, *it.MemoryInfo.SizeInMiB, "%s memory", name)
	}

	// Sapphire Rapids for c7i
	sapphireTypes := generateForGeneration(genIntelSapphireRapids, "x86_64")
	it, ok := sapphireTypes["c7i.4xlarge"]
	require.True(t, ok, "missing instance type c7i.4xlarge")
	assert.Equal(t, int64(16), *it.VCpuInfo.DefaultVCpus, "c7i.4xlarge vCPUs")
	assert.Equal(t, int64(32768), *it.MemoryInfo.SizeInMiB, "c7i.4xlarge memory")
}

func TestGenerateInstanceTypes_MemoryRatio(t *testing.T) {
	// Skylake for r5
	skylakeTypes := generateForGeneration(genIntelSkylake, "x86_64")
	expectedSkylake := map[string]struct {
		vcpus int64
		memMB int64
	}{
		"r5.large":   {2, 16384},
		"r5.xlarge":  {4, 32768},
		"r5.2xlarge": {8, 65536},
	}

	for name, exp := range expectedSkylake {
		it, ok := skylakeTypes[name]
		require.True(t, ok, "missing instance type %s", name)
		assert.Equal(t, exp.vcpus, *it.VCpuInfo.DefaultVCpus, "%s vCPUs", name)
		assert.Equal(t, exp.memMB, *it.MemoryInfo.SizeInMiB, "%s memory", name)
	}

	// Sapphire Rapids for r7i
	sapphireTypes := generateForGeneration(genIntelSapphireRapids, "x86_64")
	it, ok := sapphireTypes["r7i.4xlarge"]
	require.True(t, ok, "missing instance type r7i.4xlarge")
	assert.Equal(t, int64(16), *it.VCpuInfo.DefaultVCpus, "r7i.4xlarge vCPUs")
	assert.Equal(t, int64(131072), *it.MemoryInfo.SizeInMiB, "r7i.4xlarge memory")
}

func TestGenerateInstanceTypes_NoSmallSizesForNonBurstable(t *testing.T) {
	types := generateForGeneration(genIntelSkylake, "x86_64")

	// Non-burstable families should not have nano/micro/small/medium sizes
	for name := range types {
		if strings.HasPrefix(name, "t") {
			continue // skip all burstable families
		}
		for _, small := range []string{".nano", ".micro", ".small", ".medium"} {
			assert.False(t, strings.HasSuffix(name, small),
				"non-burstable type %s should not have %s size", name, small)
		}
	}
}

func TestGenerateInstanceTypes_OlderFamiliesHaveSmallerSizeRange(t *testing.T) {
	// Broadwell has m4 = 6 sizes
	broadwellTypes := generateForGeneration(genIntelBroadwell, "x86_64")
	assert.Equal(t, 6, countFamily(broadwellTypes, "m4."), "m4 should have 6 sizes (large → 16xlarge)")

	// Skylake has m5 = 8 sizes
	skylakeTypes := generateForGeneration(genIntelSkylake, "x86_64")
	assert.Equal(t, 8, countFamily(skylakeTypes, "m5."), "m5 should have 8 sizes (large → 24xlarge)")
}

func TestGenerateInstanceTypes_BurstableFlag(t *testing.T) {
	// Test Broadwell (has prev-gen families)
	broadwellTypes := generateForGeneration(genIntelBroadwell, "x86_64")
	prevGen := map[string]bool{"t2": true, "m4": true, "c4": true, "r4": true}

	for name, info := range broadwellTypes {
		isBurstable := strings.HasPrefix(name, "t")
		family := strings.SplitN(name, ".", 2)[0]
		assert.Equal(t, isBurstable, *info.BurstablePerformanceSupported,
			"%s burstable flag mismatch", name)
		assert.Equal(t, !prevGen[family], *info.CurrentGeneration,
			"%s current generation flag mismatch", name)
	}

	// Test current-gen (Sapphire Rapids) — all families should be currentGen=true
	sapphireTypes := generateForGeneration(genIntelSapphireRapids, "x86_64")
	for name, info := range sapphireTypes {
		isBurstable := strings.HasPrefix(name, "t")
		assert.Equal(t, isBurstable, *info.BurstablePerformanceSupported,
			"%s burstable flag mismatch", name)
		assert.True(t, *info.CurrentGeneration,
			"%s should be current generation", name)
	}
}

func TestGenerateInstanceTypes_PlacementGroupInfo(t *testing.T) {
	types := generateForGeneration(genIntelIceLake, "x86_64")
	require.NotEmpty(t, types)

	for name, info := range types {
		require.NotNil(t, info.PlacementGroupInfo,
			"%s should have PlacementGroupInfo", name)
		assert.Len(t, info.PlacementGroupInfo.SupportedStrategies, 2,
			"%s should support 2 strategies", name)

		strategies := make(map[string]bool)
		for _, s := range info.PlacementGroupInfo.SupportedStrategies {
			strategies[*s] = true
		}
		assert.True(t, strategies["cluster"], "%s should support cluster", name)
		assert.True(t, strategies["spread"], "%s should support spread", name)
	}
}

func TestGPUModelForVendorDevice_Known(t *testing.T) {
	m := GPUModelForVendorDevice("10de", "2236")
	require.NotNil(t, m, "NVIDIA A10G should be a known GPU model")
	assert.Equal(t, "g5", m.Family)
	assert.Equal(t, "NVIDIA", m.Manufacturer)
	assert.Equal(t, "A10G", m.Name)
	assert.Equal(t, int64(24576), m.MemoryMiB)
}

func TestGPUModelForVendorDevice_Unknown(t *testing.T) {
	assert.Nil(t, GPUModelForVendorDevice("dead", "beef"), "unknown PCI IDs should return nil")
	assert.Nil(t, GPUModelForVendorDevice("10de", "0000"), "wrong device ID should return nil")
}

func TestGenerateGPUTypes_NVIDIAa10g(t *testing.T) {
	types := generateGPUTypes([]GPUModel{NVIDIAa10g}, "x86_64")

	// g5 has 5 single-GPU sizes: xlarge, 2xlarge, 4xlarge, 8xlarge, 16xlarge
	assert.Len(t, types, 5)
	for _, name := range []string{"g5.xlarge", "g5.2xlarge", "g5.4xlarge", "g5.8xlarge", "g5.16xlarge"} {
		assert.True(t, hasFamily(types, name), "expected %s", name)
	}

	it, ok := types["g5.xlarge"]
	require.True(t, ok)
	assert.Equal(t, int64(4), *it.VCpuInfo.DefaultVCpus)
	assert.Equal(t, int64(16384), *it.MemoryInfo.SizeInMiB)
	require.NotNil(t, it.GpuInfo)
	require.Len(t, it.GpuInfo.Gpus, 1)
	assert.Equal(t, int64(1), *it.GpuInfo.Gpus[0].Count)
	assert.Equal(t, "NVIDIA", *it.GpuInfo.Gpus[0].Manufacturer)
	assert.Equal(t, "A10G", *it.GpuInfo.Gpus[0].Name)
	assert.Equal(t, int64(24576), *it.GpuInfo.Gpus[0].MemoryInfo.SizeInMiB)
	assert.Equal(t, int64(24576), *it.GpuInfo.TotalGpuMemoryInMiB)
	assert.False(t, *it.BurstablePerformanceSupported)
	assert.True(t, *it.CurrentGeneration)
}

func TestGenerateGPUTypes_DeduplicatesSameFamily(t *testing.T) {
	// Two GPUs of the same model (same family) should produce only one set of types.
	types := generateGPUTypes([]GPUModel{NVIDIAa10g, NVIDIAa10g}, "x86_64")
	assert.Len(t, types, 5, "duplicate GPU model should not double the type count")
}

func TestGenerateGPUTypes_EmptyModels(t *testing.T) {
	types := generateGPUTypes(nil, "x86_64")
	assert.Empty(t, types)
}

func TestIsGPUType(t *testing.T) {
	gpuTypes := generateGPUTypes([]GPUModel{NVIDIAa10g}, "x86_64")
	for name, info := range gpuTypes {
		assert.True(t, IsGPUType(info), "%s should be a GPU type", name)
	}

	cpuTypes := generateForGeneration(genIntelSkylake, "x86_64")
	for name, info := range cpuTypes {
		assert.False(t, IsGPUType(info), "%s should not be a GPU type", name)
	}
}

func TestDetectAndGenerate_WithGPUModels(t *testing.T) {
	cpu := &mockCPU{vendorID: cpuid.Intel, family: 6, model: 143} // Sapphire Rapids
	types := DetectAndGenerate(cpu, "x86_64", []GPUModel{NVIDIAa10g})

	assert.True(t, hasFamily(types, "c7i."), "should include CPU types")
	assert.True(t, hasFamily(types, "g5."), "should include GPU types when models provided")
}

func TestDetectAndGenerate_WithoutGPUModels(t *testing.T) {
	cpu := &mockCPU{vendorID: cpuid.Intel, family: 6, model: 143}
	types := DetectAndGenerate(cpu, "x86_64", nil)

	assert.True(t, hasFamily(types, "c7i."), "should include CPU types")
	assert.False(t, hasFamily(types, "g5."), "should not include GPU types when no models provided")
}

func TestGenerateGPUTypes_CPUFamiliesNotIncluded(t *testing.T) {
	// GPU generation must not contaminate CPU-only generation output.
	cpuTypes := generateForGeneration(genIntelSapphireRapids, "x86_64")
	assert.False(t, hasFamily(cpuTypes, "g5."), "CPU generation must not emit g5 types")
}
