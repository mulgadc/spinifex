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

// generateForGeneration creates the instance type map for the given CPU generation.
// It generates all instance families matching the generation's family list across
// burstable, general purpose, compute optimized, and memory optimized categories.
func generateForGeneration(gen cpuGeneration, arch string) map[string]*ec2.InstanceTypeInfo {
	// Build a set of allowed families for fast lookup
	allowed := make(map[string]bool, len(gen.families))
	for _, f := range gen.families {
		allowed[f] = true
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

// generateGPUTypes generates instance types for the discovered GPU models.
// For each unique GPU family, it emits all sizes for that family with GpuInfo populated.
// Duplicates (multiple GPUs of the same model) produce only one set of types.
// GenerateGPUTypes returns InstanceTypeInfo entries for each GPU model with GpuInfo populated.
// It is exported for use by the daemon's hot-reload path.
func GenerateGPUTypes(models []GPUModel, arch string) map[string]*ec2.InstanceTypeInfo {
	return generateGPUTypes(models, arch)
}

func generateGPUTypes(models []GPUModel, arch string) map[string]*ec2.InstanceTypeInfo {
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
							Count:        aws.Int64(1),
							Manufacturer: aws.String(model.Manufacturer),
							Name:         aws.String(model.Name),
							MemoryInfo: &ec2.GpuDeviceMemoryInfo{
								SizeInMiB: aws.Int64(model.MemoryMiB),
							},
						}},
						TotalGpuMemoryInMiB: aws.Int64(model.MemoryMiB),
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
		maps.Copy(types, generateGPUTypes(gpuModels, arch))
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
