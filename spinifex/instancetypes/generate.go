package instancetypes

import (
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// IsSystemType returns true if the instance type name is a system-internal type
// (e.g. "sys.micro") that should not be exposed via DescribeInstanceTypes.
func IsSystemType(name string) bool {
	return strings.HasPrefix(name, "sys.")
}

// SpecForSystemType returns the vCPU and memory (GB) footprint of a sys.* type.
// ok is false for unknown or non-system types.
func SpecForSystemType(name string) (vcpu int, memGB float64, ok bool) {
	if !IsSystemType(name) {
		return 0, 0, false
	}
	it, found := generateSystemTypes(runtime.GOARCH)[name]
	if !found {
		return 0, 0, false
	}
	return int(aws.Int64Value(it.VCpuInfo.DefaultVCpus)),
		float64(aws.Int64Value(it.MemoryInfo.SizeInMiB)) / 1024,
		true
}

// defaultVCPUsByInstanceType maps every instance type name to its default vCPU
// count. vCPUs depend only on the size suffix, not the CPU generation or
// architecture, so this static map covers every family the cluster can run,
// including ones a given host cannot itself detect.
var defaultVCPUsByInstanceType = func() map[string]int {
	m := make(map[string]int)
	for _, def := range instanceFamilyDefs {
		for _, size := range def.sizes {
			m[def.name+"."+size.suffix] = size.vcpus
		}
	}
	return m
}()

// DefaultVCPUs returns the default vCPU count for an instance type name (e.g.
// "c5.large"); ok is false for an unknown type. The result is independent of
// host CPU generation, so a gateway can size any account's instances even for
// families its own host cannot run.
func DefaultVCPUs(instanceType string) (vcpus int, ok bool) {
	v, ok := defaultVCPUsByInstanceType[instanceType]
	return v, ok
}

// defaultMemoryMiBByInstanceType is the memory counterpart of the vCPU map, and
// is host-independent for the same reason.
var defaultMemoryMiBByInstanceType = func() map[string]int64 {
	m := make(map[string]int64)
	for _, def := range instanceFamilyDefs {
		for _, size := range def.sizes {
			m[def.name+"."+size.suffix] = int64(size.memoryGB * 1024)
		}
	}
	return m
}()

// DefaultMemoryMiB returns the memory footprint of an instance type name; ok is
// false for an unknown type. Callers that size a guest's own configuration —
// RDS derives its engine memory parameters from it — need the figure without
// generating the whole InstanceTypeInfo the host happens to support.
func DefaultMemoryMiB(instanceType string) (memoryMiB int64, ok bool) {
	v, ok := defaultMemoryMiBByInstanceType[instanceType]
	return v, ok
}

// generateForGeneration creates instance types for the given CPU generation.
// Cross-vendor siblings are included on x86_64 so a mixed Intel+AMD cluster
// can serve either vendor family.
func generateForGeneration(gen cpuGeneration, arch string) map[string]*ec2.InstanceTypeInfo {
	allowed := make(map[string]bool, len(gen.families)*2)
	for _, f := range gen.families {
		allowed[f] = true
		if sib, ok := vendorSiblingFamily[f]; ok {
			allowed[sib] = true
		}
	}

	instanceTypes := make(map[string]*ec2.InstanceTypeInfo)
	for _, def := range instanceFamilyDefs {
		if !allowed[def.name] {
			continue
		}
		burstable := strings.HasPrefix(def.name, "t")
		for _, size := range def.sizes {
			name := fmt.Sprintf("%s.%s", def.name, size.suffix)
			instanceTypes[name] = &ec2.InstanceTypeInfo{
				InstanceType: aws.String(name),
				VCpuInfo: &ec2.VCpuInfo{
					DefaultVCpus: aws.Int64(int64(size.vcpus)),
				},
				MemoryInfo: &ec2.MemoryInfo{
					SizeInMiB: aws.Int64(int64(size.memoryGB * 1024)),
				},
				ProcessorInfo: &ec2.ProcessorInfo{
					SupportedArchitectures: []*string{aws.String(arch)},
				},
				CurrentGeneration:             aws.Bool(def.currentGen),
				BurstablePerformanceSupported: aws.Bool(burstable),
				Hypervisor:                    aws.String("kvm"),
				SupportedVirtualizationTypes:  []*string{aws.String("hvm")},
				SupportedRootDeviceTypes:      []*string{aws.String("ebs")},
				NetworkInfo:                   NetworkInfoForType(name),
				PlacementGroupInfo: &ec2.PlacementGroupInfo{
					SupportedStrategies: []*string{
						aws.String("cluster"),
						aws.String("spread"),
					},
				},
			}
		}
	}
	return instanceTypes
}

// generateSystemTypes creates the instance type map for system-internal types
// (e.g. sys.micro). These are always generated regardless of CPU generation.
func generateSystemTypes(arch string) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo)
	for _, def := range instanceFamilyDefs {
		if !IsSystemType(def.name + ".") {
			continue
		}
		for _, size := range def.sizes {
			name := fmt.Sprintf("%s.%s", def.name, size.suffix)
			types[name] = &ec2.InstanceTypeInfo{
				InstanceType: aws.String(name),
				VCpuInfo: &ec2.VCpuInfo{
					DefaultVCpus: aws.Int64(int64(size.vcpus)),
				},
				MemoryInfo: &ec2.MemoryInfo{
					SizeInMiB: aws.Int64(int64(size.memoryGB * 1024)),
				},
				ProcessorInfo: &ec2.ProcessorInfo{
					SupportedArchitectures: []*string{aws.String(arch)},
				},
				CurrentGeneration:             aws.Bool(def.currentGen),
				BurstablePerformanceSupported: aws.Bool(false),
				Hypervisor:                    aws.String("kvm"),
				SupportedVirtualizationTypes:  []*string{aws.String("hvm")},
				SupportedRootDeviceTypes:      []*string{aws.String("ebs")},
				NetworkInfo:                   NetworkInfoForType(name),
			}
		}
	}
	return types
}

// GenerateGPUTypes returns InstanceTypeInfo entries for each GPU model with
// GpuInfo populated, plus the built-in gpu.* family. models carries one entry
// per whole GPU on the node, so a node that resolved more than one distinct
// model offers no type above a count of 1.
func GenerateGPUTypes(models []GPUModel, arch string) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo)
	if len(models) == 0 {
		return types
	}

	maxGPUsPerInstance := MaxGPUsPerInstance
	if !gpuModelsHomogeneous(models) {
		maxGPUsPerInstance = 1
		slog.Warn("node holds more than one distinct GPU model, offering single-GPU types only",
			"models", len(models))
	}

	seen := make(map[string]bool)

	for _, model := range models {
		if seen[model.Family] {
			continue
		}
		seen[model.Family] = true

		for _, def := range instanceFamilyDefs {
			if def.name != model.Family {
				continue
			}
			for _, size := range def.sizes {
				name := fmt.Sprintf("%s.%s", def.name, size.suffix)
				if !gpuTypeOffered(name, maxGPUsPerInstance) {
					continue
				}
				types[name] = gpuInstanceTypeInfo(name, size, model, arch, def.currentGen)
			}
		}
	}

	// gpu.* is no model's family, so GenerateGPUTypes cannot reach it by the
	// match above. It advertises whichever model the node resolved.
	maps.Copy(types, generateBuiltinGPUTypes(models[0], arch, maxGPUsPerInstance))
	return types
}

// gpuModelsHomogeneous reports whether every discovered GPU advertises the same
// name and VRAM. Those are the figures that reach GpuInfo, so two devices that
// advertise identically can be handed out interchangeably — which also spares a
// legitimate mixed-SKU H100 node, where SXM and PCIe differ only by PCI ID.
func gpuModelsHomogeneous(models []GPUModel) bool {
	for _, m := range models[1:] {
		if m.Name != models[0].Name || m.MemoryMiB != models[0].MemoryMiB {
			return false
		}
	}
	return true
}

// gpuTypeOffered reports whether a node handing at most maxGPUsPerInstance GPUs
// to one instance can offer this type, logging by name when it cannot. An
// operator who sees a type silently absent has no thread to pull.
func gpuTypeOffered(name string, maxGPUsPerInstance int) bool {
	required := GPUCountForType(name)
	if required <= maxGPUsPerInstance {
		return true
	}
	slog.Info("GPU instance type not offered",
		"instanceType", name, "gpus_required", required,
		"gpus_offerable", maxGPUsPerInstance)
	return false
}

// generateBuiltinGPUTypes emits the gpu.* family for the node's resolved GPU
// model. Every figure but the model itself is static, so the family is
// identical on every node that discovers the same hardware.
func generateBuiltinGPUTypes(model GPUModel, arch string, maxGPUsPerInstance int) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo, len(builtinGPUSizes))
	for _, size := range builtinGPUSizes {
		name := builtinGPUPrefix + size.suffix
		if !gpuTypeOffered(name, maxGPUsPerInstance) {
			continue
		}
		types[name] = gpuInstanceTypeInfo(name, size, model, arch, true)
	}
	return types
}

// gpuInstanceTypeInfo builds one GPU InstanceTypeInfo. The vCPU and memory
// figures come from the size, the GPU count from the type name, and the model,
// VRAM and manufacturer from discovery.
func gpuInstanceTypeInfo(name string, size instanceSize, model GPUModel, arch string, currentGen bool) *ec2.InstanceTypeInfo {
	gpuCount := int64(GPUCountForType(name))
	return &ec2.InstanceTypeInfo{
		InstanceType: aws.String(name),
		VCpuInfo: &ec2.VCpuInfo{
			DefaultVCpus: aws.Int64(int64(size.vcpus)),
		},
		MemoryInfo: &ec2.MemoryInfo{
			SizeInMiB: aws.Int64(int64(size.memoryGB * 1024)),
		},
		ProcessorInfo: &ec2.ProcessorInfo{
			SupportedArchitectures: []*string{aws.String(arch)},
		},
		GpuInfo: &ec2.GpuInfo{
			Gpus: []*ec2.GpuDeviceInfo{{
				Count:        aws.Int64(gpuCount),
				Manufacturer: aws.String(model.Manufacturer),
				Name:         aws.String(model.Name),
				MemoryInfo: &ec2.GpuDeviceMemoryInfo{
					SizeInMiB: aws.Int64(model.MemoryMiB),
				},
			}},
			TotalGpuMemoryInMiB: aws.Int64(model.MemoryMiB * gpuCount),
		},
		CurrentGeneration:             aws.Bool(currentGen),
		BurstablePerformanceSupported: aws.Bool(false),
		Hypervisor:                    aws.String("kvm"),
		SupportedVirtualizationTypes:  []*string{aws.String("hvm")},
		SupportedRootDeviceTypes:      []*string{aws.String("ebs")},
		NetworkInfo:                   NetworkInfoForType(name),
		PlacementGroupInfo: &ec2.PlacementGroupInfo{
			SupportedStrategies: []*string{
				aws.String("cluster"),
				aws.String("spread"),
			},
		},
	}
}

// IsGPUType returns true if the instance type has GPU resources.
func IsGPUType(info *ec2.InstanceTypeInfo) bool {
	return info.GpuInfo != nil && len(info.GpuInfo.Gpus) > 0
}

// MIGProfileSpec carries the profile name and per-slice VRAM needed to generate
// MIG instance types without importing the gpu package.
type MIGProfileSpec struct {
	Name      string // nvidia-smi profile name, e.g. "1g.10gb"
	MemoryMiB int64
}

// migTypePrefix marks an instance type name as a MIG profile type.
const migTypePrefix = "mig."

// IsMIGType reports whether the instance type name is a MIG profile type
// (i.e. was produced by GenerateMIGTypes).
func IsMIGType(instanceType string) bool {
	return strings.HasPrefix(instanceType, migTypePrefix)
}

// MIGProfileFromType extracts the nvidia-smi profile name from a MIG instance
// type name (e.g. "mig.1g.10gb" → "1g.10gb"). Returns "" for non-MIG types.
func MIGProfileFromType(instanceType string) string {
	profile, ok := strings.CutPrefix(instanceType, migTypePrefix)
	if !ok {
		return ""
	}
	return profile
}

// GenerateMIGTypes returns one InstanceTypeInfo per unique MIG profile. Instance
// type names use the nvidia-smi profile name verbatim (e.g. "mig.1g.10gb").
// Duplicate profile names are silently de-duplicated.
func GenerateMIGTypes(profiles []MIGProfileSpec, arch string) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo)
	for _, p := range profiles {
		name := migTypePrefix + p.Name
		if _, exists := types[name]; exists {
			continue
		}
		vcpus, memMiB := MIGHostResources(p.Name)
		types[name] = &ec2.InstanceTypeInfo{
			InstanceType: aws.String(name),
			VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(vcpus)},
			MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(memMiB)},
			ProcessorInfo: &ec2.ProcessorInfo{
				SupportedArchitectures: []*string{aws.String(arch)},
			},
			GpuInfo: &ec2.GpuInfo{
				Gpus: []*ec2.GpuDeviceInfo{{
					Count:        aws.Int64(1),
					Manufacturer: aws.String("NVIDIA"),
					Name:         aws.String("MIG " + p.Name),
					MemoryInfo: &ec2.GpuDeviceMemoryInfo{
						SizeInMiB: aws.Int64(p.MemoryMiB),
					},
				}},
				TotalGpuMemoryInMiB: aws.Int64(p.MemoryMiB),
			},
			CurrentGeneration:             aws.Bool(true),
			BurstablePerformanceSupported: aws.Bool(false),
			Hypervisor:                    aws.String("kvm"),
			SupportedVirtualizationTypes:  []*string{aws.String("hvm")},
			SupportedRootDeviceTypes:      []*string{aws.String("ebs")},
			NetworkInfo:                   NetworkInfoForType(name),
			PlacementGroupInfo: &ec2.PlacementGroupInfo{
				SupportedStrategies: []*string{
					aws.String("cluster"),
					aws.String("spread"),
				},
			},
		}
	}
	return types
}

// DetectAndGenerate detects the host CPU generation and generates matching instance types.
// gpuModels is the list of GPU models discovered on the host; pass nil if no GPUs are present.
func DetectAndGenerate(cpu CPUInfo, arch string, gpuModels []GPUModel) map[string]*ec2.InstanceTypeInfo {
	// Normalize Go's "amd64" to the Linux/AWS convention "x86_64".
	if arch == "amd64" {
		arch = "x86_64"
	}

	gen := detectCPUGeneration(cpu, arch)
	types := generateForGeneration(gen, arch)

	// Merge in system types (always available regardless of CPU generation).
	maps.Copy(types, generateSystemTypes(arch))

	// Merge in GPU instance types if the host has recognized GPUs.
	if len(gpuModels) > 0 {
		maps.Copy(types, GenerateGPUTypes(gpuModels, arch))
	}

	if len(types) == 0 {
		slog.Error("No instance types generated, daemon will be unable to run VMs",
			"generation", gen.name, "arch", arch)
	} else {
		slog.Info("CPU generation detected",
			"generation", gen.name, "families", gen.families,
			"instanceTypes", len(types), "os", runtime.GOOS)
	}

	return types
}
