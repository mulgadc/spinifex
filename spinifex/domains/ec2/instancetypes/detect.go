package instancetypes

import (
	"log/slog"
	"strings"

	cpuid "github.com/klauspost/cpuid/v2"
)

// CPUInfo abstracts CPU identification for testability.
type CPUInfo interface {
	VendorID() cpuid.Vendor
	Family() int
	Model() int
	BrandName() string
	HasFeature(cpuid.FeatureID) bool
}

// HostCPU implements CPUInfo by reading the host's cpuid.CPU global.
type HostCPU struct{}

var _ CPUInfo = (*HostCPU)(nil)

func (HostCPU) VendorID() cpuid.Vendor            { return cpuid.CPU.VendorID }
func (HostCPU) Family() int                       { return cpuid.CPU.Family }
func (HostCPU) Model() int                        { return cpuid.CPU.Model }
func (HostCPU) BrandName() string                 { return cpuid.CPU.BrandName }
func (HostCPU) HasFeature(f cpuid.FeatureID) bool { return cpuid.CPU.Has(f) }

// detectCPUGeneration detects the CPU microarchitecture generation using CPUID.
func detectCPUGeneration(cpu CPUInfo, arch string) cpuGeneration {
	switch cpu.VendorID() {
	case cpuid.Intel:
		return detectIntelGeneration(cpu.Family(), cpu.Model())
	case cpuid.AMD:
		return detectAMDGeneration(cpu.Family(), cpu.Model())
	default:
		if arch == "arm64" {
			return detectARMGeneration(cpu)
		}
		slog.Warn("CPUID vendor not recognized, falling back to brand string detection",
			"vendorID", cpu.VendorID(), "brand", cpu.BrandName())
	}
	return detectGenerationFromBrand(cpu, arch)
}

// detectIntelGeneration maps Intel CPUID Family 6 model numbers to generations.
func detectIntelGeneration(family, model int) cpuGeneration {
	if family != 6 {
		slog.Warn("Unrecognized Intel CPU family, exposing t3 only", "family", family, "model", model)
		return genUnknownIntel
	}

	switch model {
	case 79, 86: // Broadwell server (BDX, BDX-DE)
		return genIntelBroadwell
	case 85: // Skylake-SP / Cascade Lake-SP
		return genIntelSkylake
	case 106, 108: // Ice Lake server (ICX, ICX-D)
		return genIntelIceLake
	case 143, 207: // Sapphire Rapids (SPR, EMR)
		return genIntelSapphireRapids
	case 173, 174: // Granite Rapids (GNR, GNR-D)
		return genIntelGraniteRapids

	// Consumer/desktop mapped to nearest server generation
	case 151, 154: // Alder Lake
		return genIntelIceLake
	case 183, 191: // Raptor Lake
		return genIntelSapphireRapids
	case 197, 198: // Arrow Lake
		return genIntelGraniteRapids
	}

	slog.Warn("Unrecognized Intel CPU model, exposing t3 only", "family", family, "model", model)
	return genUnknownIntel
}

// detectAMDGeneration maps AMD CPUID family/model to generations.
func detectAMDGeneration(family, model int) cpuGeneration {
	switch family {
	case 23: // Zen, Zen+, Zen 2 (Naples, Rome, Matisse, etc.)
		return genAMDZen
	case 25: // Zen 3 and Zen 4 share family 25; model ranges distinguish them
		// Zen 3 models: 0x00-0x0F (Milan/Vermeer), 0x20-0x5F (Rembrandt/Barcelo)
		// Zen 4 models: 0x10-0x1F (Genoa), 0x60+ (Raphael/Phoenix)
		isZen3 := model < 0x10 || (model >= 0x20 && model < 0x60)
		if isZen3 {
			return genAMDZen3
		}
		return genAMDZen4
	case 26: // Zen 5 (Turin, Granite Ridge)
		return genAMDZen5
	}

	slog.Warn("Unrecognized AMD CPU family, exposing t3a only", "family", family, "model", model)
	return genUnknownAMD
}

// detectARMGeneration detects ARM CPU generation using brand string and feature flags.
func detectARMGeneration(cpu CPUInfo) cpuGeneration {
	brand := strings.ToLower(cpu.BrandName())

	// Check for specific Graviton versions
	if strings.Contains(brand, "graviton4") || strings.Contains(brand, "neoverse-v2") {
		return genARMNeoverseV2
	}
	if strings.Contains(brand, "graviton3") || strings.Contains(brand, "neoverse-v1") {
		return genARMNeoverseV1
	}
	if strings.Contains(brand, "graviton2") || strings.Contains(brand, "neoverse-n1") {
		return genARMNeoverseN1
	}

	// SVE indicates Neoverse V1+ but cannot distinguish V1 from V2
	if cpu.HasFeature(cpuid.SVE) {
		slog.Warn("ARM generation detected via SVE heuristic, defaulting to Neoverse V1", "brand", cpu.BrandName())
		return genARMNeoverseV1
	}

	// Unknown ARM — expose only burstable t4g (same as unknown Intel/AMD behavior)
	slog.Warn("Could not identify ARM generation, exposing t4g only", "brand", cpu.BrandName())
	return genUnknownARM
}

// detectGenerationFromBrand is a fallback for VMs/hypervisors where CPUID may be virtualized.
func detectGenerationFromBrand(cpu CPUInfo, arch string) cpuGeneration {
	if arch == "arm64" {
		return detectARMGeneration(cpu)
	}

	brand := cpu.BrandName()
	brandLower := strings.ToLower(brand)

	// Intel patterns
	if strings.Contains(brandLower, "xeon") || strings.Contains(brandLower, "intel") {
		switch {
		case strings.Contains(brandLower, "granite"):
			return genIntelGraniteRapids
		case strings.Contains(brandLower, "sapphire"):
			return genIntelSapphireRapids
		case strings.Contains(brandLower, "ice lake") || strings.Contains(brandLower, "icelake"):
			return genIntelIceLake
		case strings.Contains(brandLower, "cascade") || strings.Contains(brandLower, "skylake"):
			return genIntelSkylake
		case strings.Contains(brandLower, "broadwell"):
			return genIntelBroadwell
		default:
			// Generic Intel — default to Skylake (most common in VMs)
			slog.Warn("Intel CPU detected via brand string but generation unknown, defaulting to Skylake", "brand", brand)
			return genIntelSkylake
		}
	}

	// AMD patterns
	if strings.Contains(brandLower, "epyc") || strings.Contains(brandLower, "amd") || strings.Contains(brandLower, "ryzen") {
		switch {
		case strings.Contains(brandLower, "turin"):
			return genAMDZen5
		case strings.Contains(brandLower, "genoa") || strings.Contains(brandLower, "9004") || strings.Contains(brandLower, "raphael"):
			return genAMDZen4
		case strings.Contains(brandLower, "milan") || strings.Contains(brandLower, "7003") || strings.Contains(brandLower, "vermeer"):
			return genAMDZen3
		default:
			// Generic AMD — default to Zen/Zen2
			slog.Warn("AMD CPU detected via brand string but generation unknown, defaulting to Zen/Zen2", "brand", brand)
			return genAMDZen
		}
	}

	slog.Warn("Unrecognized CPU, exposing t3 only", "brand", brand, "arch", arch)
	return genUnknown
}
