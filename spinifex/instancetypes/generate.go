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
			}
		}
	}
	return types
}

// GenerateGPUTypes returns InstanceTypeInfo entries for each GPU model with GpuInfo populated.
func GenerateGPUTypes(models []GPUModel, arch string) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo)
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
				gpuCount := int64(GPUCountForType(name))
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
					CurrentGeneration:             aws.Bool(def.currentGen),
					BurstablePerformanceSupported: aws.Bool(false),
					Hypervisor:                    aws.String("kvm"),
					SupportedVirtualizationTypes:  []*string{aws.String("hvm")},
					SupportedRootDeviceTypes:      []*string{aws.String("ebs")},
					PlacementGroupInfo: &ec2.PlacementGroupInfo{
						SupportedStrategies: []*string{
							aws.String("cluster"),
							aws.String("spread"),
						},
					},
				}
			}
		}
	}
	return types
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

// IsMIGType reports whether the instance type name is a MIG profile type
// (i.e. was produced by GenerateMIGTypes).
func IsMIGType(instanceType string) bool {
	return strings.HasPrefix(instanceType, "mig.")
}

// MIGProfileFromType extracts the nvidia-smi profile name from a MIG instance
// type name (e.g. "mig.1g.10gb" → "1g.10gb"). Returns "" for non-MIG types.
func MIGProfileFromType(instanceType string) string {
	if !IsMIGType(instanceType) {
		return ""
	}
	return strings.TrimPrefix(instanceType, "mig.")
}

// GenerateMIGTypes returns one InstanceTypeInfo per unique MIG profile. Instance
// type names use the nvidia-smi profile name verbatim (e.g. "mig.1g.10gb").
// Duplicate profile names are silently de-duplicated.
func GenerateMIGTypes(profiles []MIGProfileSpec, arch string) map[string]*ec2.InstanceTypeInfo {
	types := make(map[string]*ec2.InstanceTypeInfo)
	for _, p := range profiles {
		name := "mig." + p.Name
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
