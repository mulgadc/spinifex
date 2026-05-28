package handlers_ec2_instance

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
)

// InstanceService defines the interface for EC2 instance operations business logic
type InstanceService interface {
	RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	DescribeInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceStatus(input *ec2.DescribeInstanceStatusInput, accountID string) (*ec2.DescribeInstanceStatusOutput, error)
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
//
// GetAvailableInstanceTypeInfos reports types gated by free schedulable
// capacity (one entry per free slot when showCapacity is true). It is used
// when callers ask "what could you launch right now?" — typically with the
// capacity=true filter for aggregated cluster-wide accounting.
//
// GetSupportedInstanceTypeInfos reports every type this node is configured
// to run, regardless of current allocation. This is the AWS-compatible
// answer for plain DescribeInstanceTypes and is what tooling like the
// Terraform AWS provider expects when it performs a metadata lookup after
// RunInstances.
type ResourceCapacityProvider interface {
	GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo
	GetSupportedInstanceTypeInfos() []*ec2.InstanceTypeInfo
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

// GPUClaimer binds a GPU for a starting instance and returns an attachment
// descriptor. For whole-GPU passthrough the descriptor carries the PCI address;
// for MIG slices it carries the mdev path. nil claimer means no GPU passthrough.
type GPUClaimer interface {
	Claim(instanceID, profileName string) (*gpu.GPUAttachment, error)
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

// AMIMetaLoader resolves an AMI ID to its metadata for ownership/validation
// during RunInstances. Implemented by handlers/ec2/image.ImageServiceImpl.
type AMIMetaLoader interface {
	GetAMIConfig(imageID string) (viperblock.AMIMetadata, error)
}

// KeyPairValidator checks that a named key pair exists for an account during
// RunInstances. Implemented by handlers/ec2/key.KeyServiceImpl.
type KeyPairValidator interface {
	ValidateKeyPairExists(accountID, keyName string) error
}

// SubnetInfo carries the subset of subnet metadata RunInstances needs to
// resolve default subnets and decide whether to auto-assign a public IP.
type SubnetInfo struct {
	SubnetID            string
	VpcID               string
	MapPublicIpOnLaunch bool
}

// ENICreator covers the VPC/ENI operations RunInstances performs while
// auto-attaching a primary interface. Implemented via an adapter over
// handlers/ec2/vpc.VPCServiceImpl on the daemon. DetachENI lives here (not on
// ENIDeleter) because the launch-time NAT-rollback path needs to flip the ENI
// to "available" before ENIDeleter.DeleteNetworkInterface, which rejects
// in-use ENIs.
type ENICreator interface {
	GetDefaultSubnet(accountID string) (*SubnetInfo, error)
	GetSubnet(accountID, subnetID string) (*SubnetInfo, error)
	CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	AttachENI(accountID, eniID, instanceID string, deviceIndex int64) (string, error)
	DetachENI(accountID, eniID string) error
	UpdateENIPublicIP(accountID, eniID, publicIP, poolName string) error
}

// PublicIPAllocator allocates a public IP to an instance/ENI from a pool.
// Implemented by handlers/ec2/vpc.ExternalIPAM.
type PublicIPAllocator interface {
	AllocateIP(region, az, allocType, allocID, eniID, instanceID string) (publicIP, poolName string, err error)
}
