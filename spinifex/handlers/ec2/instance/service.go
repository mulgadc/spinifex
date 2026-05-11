package handlers_ec2_instance

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceService defines the interface for EC2 instance operations business logic
type InstanceService interface {
	RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	DescribeInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(input *ec2.DescribeInstanceTypesInput, accountID string) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeInstanceAttribute(input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error)
	DescribeStoppedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	DescribeTerminatedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	ModifyInstanceAttribute(input *ec2.ModifyInstanceAttributeInput, accountID string) (*ec2.ModifyInstanceAttributeOutput, error)
	StartStoppedInstance(input *StartStoppedInstanceInput, accountID string) (*StartStoppedInstanceOutput, error)
	TerminateStoppedInstance(input *TerminateStoppedInstanceInput, accountID string) (*TerminateStoppedInstanceOutput, error)
}

// StartStoppedInstanceInput is the payload for ec2.StartStoppedInstance.
type StartStoppedInstanceInput struct {
	InstanceID string `json:"instance_id"`
}

// StartStoppedInstanceOutput is the response payload.
type StartStoppedInstanceOutput struct {
	Status     string `json:"status"`
	InstanceID string `json:"instanceId"`
}

// TerminateStoppedInstanceInput is the payload for ec2.TerminateStoppedInstance.
type TerminateStoppedInstanceInput struct {
	InstanceID string `json:"instance_id"`
}

// TerminateStoppedInstanceOutput is the response payload.
type TerminateStoppedInstanceOutput struct {
	Status     string `json:"status"`
	InstanceID string `json:"instanceId"`
}

// ResourceCapacityProvider exposes per-node instance-type availability used by
// DescribeInstanceTypes. Implemented by daemon.ResourceManager.
type ResourceCapacityProvider interface {
	GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo
}

// InstanceTypeAllocator extends ResourceCapacityProvider with the mutating
// resource-reservation methods used by StartStoppedInstance. Implemented by
// daemon.ResourceManager.
type InstanceTypeAllocator interface {
	ResourceCapacityProvider
	Allocate(instanceType *ec2.InstanceTypeInfo) error
	Deallocate(instanceType *ec2.InstanceTypeInfo)
	CanAllocate(instanceType *ec2.InstanceTypeInfo, count int) int
	InstanceTypes() map[string]*ec2.InstanceTypeInfo
}

// GPUClaimer binds a GPU (and its IOMMU group) to vfio-pci for a starting
// instance. Returns the PCI address and whether QEMU should set x-vga=on.
// nil claimer means the daemon has no GPU passthrough configured.
type GPUClaimer interface {
	Claim(instanceID string) (pciAddress string, xvgaEnabled bool, err error)
	Release(instanceID string) error
}

// StoppedInstanceStore covers KV-backed read/write access for stopped instances
// and read+write access for terminated instances. Implemented by
// daemon.JetStreamManager.
type StoppedInstanceStore interface {
	LoadStoppedInstance(instanceID string) (*vm.VM, error)
	ListStoppedInstances() ([]*vm.VM, error)
	ListTerminatedInstances() ([]*vm.VM, error)
	WriteStoppedInstance(instanceID string, instance *vm.VM) error
	DeleteStoppedInstance(instanceID string) error
	WriteTerminatedInstance(instanceID string, instance *vm.VM) error
}

// VolumeDeleter deletes EBS volumes. Implemented by handlers/ec2/volume's
// VolumeServiceImpl (used by the daemon).
type VolumeDeleter interface {
	DeleteVolume(input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error)
}

// ENIDeleter deletes ENIs. Implemented by handlers/ec2/vpc's VPCServiceImpl.
type ENIDeleter interface {
	DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
}

// PublicIPReleaser releases a previously allocated public IP back to a pool.
// Implemented by handlers/ec2/vpc.ExternalIPAM.
type PublicIPReleaser interface {
	ReleaseIP(pool, ip string) error
}
