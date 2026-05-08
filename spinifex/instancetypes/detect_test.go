package instancetypes

import (
	"runtime"
	"testing"

	cpuid "github.com/klauspost/cpuid/v2"
	"github.com/stretchr/testify/assert"
)

func TestDetectIntelGeneration(t *testing.T) {
	tests := []struct {
		name     string
		family   int
		model    int
		expected cpuGeneration
	}{
		{"Broadwell BDX", 6, 79, genIntelBroadwell},
		{"Broadwell BDX-DE", 6, 86, genIntelBroadwell},
		{"Skylake-SP", 6, 85, genIntelSkylake},
		{"Ice Lake ICX", 6, 106, genIntelIceLake},
		{"Ice Lake ICX-D", 6, 108, genIntelIceLake},
		{"Sapphire Rapids SPR", 6, 143, genIntelSapphireRapids},
		{"Emerald Rapids EMR", 6, 207, genIntelSapphireRapids},
		{"Granite Rapids GNR", 6, 173, genIntelGraniteRapids},
		{"Granite Rapids GNR-D", 6, 174, genIntelGraniteRapids},
		// Consumer mappings
		{"Alder Lake", 6, 151, genIntelIceLake},
		{"Alder Lake P", 6, 154, genIntelIceLake},
		{"Raptor Lake", 6, 183, genIntelSapphireRapids},
		{"Raptor Lake P", 6, 191, genIntelSapphireRapids},
		{"Arrow Lake", 6, 197, genIntelGraniteRapids},
		{"Arrow Lake S", 6, 198, genIntelGraniteRapids},
		// Unknown
		{"Unknown family", 15, 0, genUnknownIntel},
		{"Unknown model", 6, 255, genUnknownIntel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen := detectIntelGeneration(tt.family, tt.model)
			assert.Equal(t, tt.expected.name, gen.name)
			assert.Equal(t, tt.expected.families, gen.families)
		})
	}
}

func TestDetectAMDGeneration(t *testing.T) {
	tests := []struct {
		name     string
		family   int
		model    int
		expected cpuGeneration
	}{
		{"Naples/Rome family 23", 23, 1, genAMDZen},
		{"Zen3 Milan model 0x01", 25, 0x01, genAMDZen3},
		{"Zen3 Vermeer model 0x21", 25, 0x21, genAMDZen3},
		{"Zen4 Genoa model 0x11", 25, 0x11, genAMDZen4},
		{"Zen4 Raphael model 0x61", 25, 0x61, genAMDZen4},
		// Boundary tests for family 25 Zen3/Zen4 split
		{"Zen3 boundary 0x0F", 25, 0x0F, genAMDZen3},
		{"Zen4 boundary 0x10", 25, 0x10, genAMDZen4},
		{"Zen4 boundary 0x1F", 25, 0x1F, genAMDZen4},
		{"Zen3 boundary 0x20", 25, 0x20, genAMDZen3},
		{"Zen3 boundary 0x5F", 25, 0x5F, genAMDZen3},
		{"Zen4 boundary 0x60", 25, 0x60, genAMDZen4},
		{"Zen4 max model", 25, 0xFF, genAMDZen4},
		{"Zen5 Turin family 26", 26, 0, genAMDZen5},
		{"Unknown AMD family", 20, 0, genUnknownAMD},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen := detectAMDGeneration(tt.family, tt.model)
			assert.Equal(t, tt.expected.name, gen.name)
			assert.Equal(t, tt.expected.families, gen.families)
		})
	}
}

func TestDetectGenerationFromBrand(t *testing.T) {
	tests := []struct {
		name     string
		brand    string
		arch     string
		expected cpuGeneration
	}{
		{"Intel Skylake brand", "Intel(R) Xeon(R) Platinum 8175M (Skylake)", "x86_64", genIntelSkylake},
		{"Intel Ice Lake brand", "Intel(R) Xeon(R) Platinum 8375C (Ice Lake)", "x86_64", genIntelIceLake},
		{"Intel Sapphire brand", "Intel(R) Xeon(R) w9-3495X (Sapphire Rapids)", "x86_64", genIntelSapphireRapids},
		{"Intel Granite brand", "Intel(R) Xeon(R) 6980P (Granite Rapids)", "x86_64", genIntelGraniteRapids},
		{"Intel Broadwell brand", "Intel(R) Xeon(R) E5-2686 v4 (Broadwell)", "x86_64", genIntelBroadwell},
		{"Generic Intel Xeon", "Intel(R) Xeon(R) CPU E5-2686 v4", "x86_64", genIntelSkylake}, // defaults to Skylake
		{"AMD Milan brand", "AMD EPYC 7003 Milan", "x86_64", genAMDZen3},
		{"AMD Genoa brand", "AMD EPYC 9004 Series", "x86_64", genAMDZen4},
		{"Generic AMD EPYC", "AMD EPYC 7551", "x86_64", genAMDZen},
		{"Completely unknown", "Some Random CPU", "x86_64", genUnknown},
		// ARM delegation path
		{"ARM via brand fallback", "AWS Graviton4", "arm64", genARMNeoverseV2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCPU{brandName: tt.brand}
			gen := detectGenerationFromBrand(mock, tt.arch)
			assert.Equal(t, tt.expected.name, gen.name)
			assert.Equal(t, tt.expected.families, gen.families)
		})
	}
}

func TestCPUDetection(t *testing.T) {
	cpu := HostCPU{}
	gen := detectCPUGeneration(cpu, hostArch())
	assert.NotEmpty(t, gen.name, "Generation name should not be empty")
	assert.NotEmpty(t, gen.families, "Generation families should not be empty")
	t.Logf("Detected CPU generation: %s, families: %v", gen.name, gen.families)
}

// --- Tests using mock CPUInfo ---

type mockCPU struct {
	vendorID  cpuid.Vendor
	family    int
	model     int
	brandName string
	features  map[cpuid.FeatureID]bool
}

func (m *mockCPU) VendorID() cpuid.Vendor            { return m.vendorID }
func (m *mockCPU) Family() int                       { return m.family }
func (m *mockCPU) Model() int                        { return m.model }
func (m *mockCPU) BrandName() string                 { return m.brandName }
func (m *mockCPU) HasFeature(f cpuid.FeatureID) bool { return m.features[f] }

func TestDetectCPUGeneration_IntelViaInterface(t *testing.T) {
	cpu := &mockCPU{
		vendorID: cpuid.Intel,
		family:   6,
		model:    143, // Sapphire Rapids
	}
	gen := detectCPUGeneration(cpu, "x86_64")
	assert.Equal(t, genIntelSapphireRapids.name, gen.name)
	assert.Equal(t, genIntelSapphireRapids.families, gen.families)
}

func TestDetectCPUGeneration_AMDViaInterface(t *testing.T) {
	cpu := &mockCPU{
		vendorID: cpuid.AMD,
		family:   25,
		model:    0x11, // Genoa (Zen 4)
	}
	gen := detectCPUGeneration(cpu, "x86_64")
	assert.Equal(t, genAMDZen4.name, gen.name)
	assert.Equal(t, genAMDZen4.families, gen.families)
}

func TestDetectCPUGeneration_ARMViaInterface(t *testing.T) {
	cpu := &mockCPU{
		vendorID:  0, // unknown vendor
		brandName: "AWS Graviton3",
		features:  map[cpuid.FeatureID]bool{cpuid.SVE: true},
	}
	gen := detectCPUGeneration(cpu, "arm64")
	assert.Equal(t, genARMNeoverseV1.name, gen.name)
	assert.Equal(t, genARMNeoverseV1.families, gen.families)
}

func TestDetectCPUGeneration_ARMUnknown(t *testing.T) {
	cpu := &mockCPU{
		vendorID:  0, // unknown vendor
		brandName: "Some Unknown ARM Processor",
	}
	gen := detectCPUGeneration(cpu, "arm64")
	assert.Equal(t, genUnknownARM.name, gen.name)
	assert.Equal(t, genUnknownARM.families, gen.families)
}

func TestDetectCPUGeneration_FallbackToBrand(t *testing.T) {
	cpu := &mockCPU{
		vendorID:  0, // unknown vendor
		brandName: "AMD EPYC 9004 Series",
	}
	gen := detectCPUGeneration(cpu, "x86_64")
	assert.Equal(t, genAMDZen4.name, gen.name)
	assert.Equal(t, genAMDZen4.families, gen.families)
}

// --- detectARMGeneration coverage for all 5 code paths ---

func TestDetectARMGeneration(t *testing.T) {
	tests := []struct {
		name     string
		brand    string
		features map[cpuid.FeatureID]bool
		expected cpuGeneration
	}{
		{"Graviton4 brand", "AWS Graviton4", nil, genARMNeoverseV2},
		{"Neoverse V2 brand", "ARM Neoverse-V2", nil, genARMNeoverseV2},
		{"Graviton3 brand", "AWS Graviton3", nil, genARMNeoverseV1},
		{"Neoverse V1 brand", "ARM Neoverse-V1", nil, genARMNeoverseV1},
		{"Graviton2 brand", "AWS Graviton2", nil, genARMNeoverseN1},
		{"Neoverse N1 brand", "ARM Neoverse-N1", nil, genARMNeoverseN1},
		{"SVE only, no brand match", "Generic ARM Processor", map[cpuid.FeatureID]bool{cpuid.SVE: true}, genARMNeoverseV1},
		{"Unknown ARM, no SVE", "Generic ARM Processor", nil, genUnknownARM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu := &mockCPU{brandName: tt.brand, features: tt.features}
			gen := detectARMGeneration(cpu)
			assert.Equal(t, tt.expected.name, gen.name)
			assert.Equal(t, tt.expected.families, gen.families)
		})
	}
}

// --- DetectAndGenerate (public API) ---

func TestDetectAndGenerate_Intel(t *testing.T) {
	cpu := &mockCPU{vendorID: cpuid.Intel, family: 6, model: 143}
	types := DetectAndGenerate(cpu, "x86_64", nil)
	assert.NotEmpty(t, types)
	assert.True(t, hasFamily(types, "c7i."), "Sapphire Rapids should include c7i")
}

func TestDetectAndGenerate_NormalizesAmd64(t *testing.T) {
	cpu := &mockCPU{vendorID: cpuid.Intel, family: 6, model: 106}
	types := DetectAndGenerate(cpu, "amd64", nil)
	assert.NotEmpty(t, types)
	assert.True(t, hasFamily(types, "c6i."), "amd64 should normalize to x86_64 and produce Ice Lake types")
}

func hostArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x86_64"
}
