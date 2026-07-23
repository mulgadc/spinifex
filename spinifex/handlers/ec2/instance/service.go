package handlers_ec2_instance

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
)

// InstanceService defines the interface for EC2 instance operations business logic.
type InstanceService interface {
	RunInstances(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceStatus(ctx context.Context, input *ec2.DescribeInstanceStatusInput, accountID string) (*ec2.DescribeInstanceStatusOutput, error)
	DescribeInstanceTypes(ctx context.Context, input *ec2.DescribeInstanceTypesInput, accountID string) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeInstanceAttribute(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error)
	DescribeStoppedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	DescribeTerminatedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	ModifyInstanceAttribute(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, accountID string) (*ec2.ModifyInstanceAttributeOutput, error)
	ModifyInstanceMetadataOptions(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, accountID string) (*ec2.ModifyInstanceMetadataOptionsOutput, error)
	StartStoppedInstance(ctx context.Context, input *StartStoppedInstanceInput, accountID string) (*StartStoppedInstanceOutput, error)
	TerminateStoppedInstance(ctx context.Context, input *TerminateStoppedInstanceInput, accountID string) (*TerminateStoppedInstanceOutput, error)
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

// ResourceCapacityProvider exposes per-node instance-type availability.
// GetAvailableInstanceTypeInfos gates on free capacity; GetSupportedInstanceTypeInfos
// returns all configured types regardless of allocation.
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
	AllocateFromReservation(reservationID, accountID string, instanceType *ec2.InstanceTypeInfo) error
	ReleaseToReservation(reservationID string, instanceType *ec2.InstanceTypeInfo)
	ReservationAvailable(reservationID, accountID string, instanceType *ec2.InstanceTypeInfo) int
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
	// UpdateStoppedInstance atomically applies mutate to the current stopped
	// record under optimistic concurrency (CAS) with createIfAbsent=false, so
	// a caller racing a winning ClaimStoppedInstance gets a clean
	// nats.ErrKeyNotFound instead of resurrecting a deleted record.
	UpdateStoppedInstance(instanceID string, mutate func(*vm.VM)) (*vm.VM, error)
	WriteTerminatedInstance(instanceID string, instance *vm.VM) error
	// ClaimStoppedInstance atomically removes instanceID's record and
	// returns the VM it held, so at most one caller can ever win a race to
	// (re)launch the same stopped instance. Returns vm.ErrStoppedInstanceClaimed
	// if a concurrent caller already claimed (or otherwise removed) the record.
	ClaimStoppedInstance(instanceID string) (*vm.VM, error)
}

// InstanceTagWriter projects an instance record's full tag set into the
// central tag store, and removes it on terminate. Implemented by
// handlers/ec2/tags.TagsServiceImpl.
type InstanceTagWriter interface {
	PutResourceTags(ctx context.Context, accountID, resourceID string, tags map[string]string) error
	DeleteAllTags(ctx context.Context, accountID, resourceID string) error
}

// VolumeDeleter deletes EBS volumes. Implemented by handlers/ec2/volume's
// VolumeServiceImpl (used by the daemon).
type VolumeDeleter interface {
	DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error)

	// DeleteVolumeOnTerminate deletes a DeleteOnTermination volume as part of
	// an instance terminate: it clears any stale attachment (terminate
	// implies detach) before deleting, so a still-attached boot volume left
	// over from Stop is not rejected by DeleteVolume's in-use guard.
	DeleteVolumeOnTerminate(ctx context.Context, volumeID, accountID string) error

	// DetachVolumeOnTerminate clears a still-attached volume's attachment on
	// terminate without deleting it, matching AWS semantics for a
	// DeleteOnTermination=false volume: terminate still implies detach, it
	// just leaves the volume behind as available rather than deleting it.
	DetachVolumeOnTerminate(ctx context.Context, volumeID, accountID string) error
}

// ENIDeleter deletes ENIs. Implemented by handlers/ec2/vpc's VPCServiceImpl.
type ENIDeleter interface {
	DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
}

// PublicIPReleaser releases a previously allocated public IP back to a pool.
// Implemented by handlers/ec2/vpc.ExternalIPAM. ownerENIID scopes the release
// to the ENI that owns the lease so a stale teardown for a recycled IP no-ops.
type PublicIPReleaser interface {
	ReleaseIP(ctx context.Context, pool, ip, ownerENIID string) error
}

// AMIMetaLoader resolves an AMI ID to its metadata for ownership/validation
// during RunInstances. Implemented by handlers/ec2/image.ImageServiceImpl.
type AMIMetaLoader interface {
	GetAMIConfig(ctx context.Context, imageID string) (viperblock.AMIMetadata, error)
}

// KeyPairValidator checks that a named key pair exists for an account during
// RunInstances. Implemented by handlers/ec2/key.KeyServiceImpl.
type KeyPairValidator interface {
	ValidateKeyPairExists(ctx context.Context, accountID, keyName string) error
}

// SubnetInfo carries the subset of subnet metadata RunInstances needs to
// resolve default subnets and decide whether to auto-assign a public IP.
type SubnetInfo struct {
	SubnetID            string
	VpcID               string
	MapPublicIpOnLaunch bool
}

// ENIInfo carries the ENI metadata RunInstances needs for a pre-created primary
// interface. Translated from handlers/ec2/vpc.ENIRecord to avoid a cyclic import.
type ENIInfo struct {
	NetworkInterfaceID string
	SubnetID           string
	VpcID              string
	PrivateIpAddress   string
	MacAddress         string
	Status             string
	SecurityGroupIDs   []string
}

// ENICreator covers the VPC/ENI operations RunInstances uses to auto-attach a
// primary interface. DetachENI is here (not ENIDeleter) so the NAT-rollback
// path can flip the ENI to "available" before deletion.
type ENICreator interface {
	GetDefaultSubnet(ctx context.Context, accountID string) (*SubnetInfo, error)
	GetSubnet(ctx context.Context, accountID, subnetID string) (*SubnetInfo, error)
	GetENI(ctx context.Context, accountID, eniID string) (*ENIInfo, error)
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	AttachENI(ctx context.Context, accountID, eniID, instanceID string, deviceIndex int64) (string, error)
	DetachENI(ctx context.Context, accountID, eniID string) error
	UpdateENIPublicIP(ctx context.Context, accountID, eniID, publicIP, poolName string) error
}

// PublicIPAllocator allocates a public IP to an instance/ENI from a pool.
// Implemented by handlers/ec2/vpc.ExternalIPAM.
type PublicIPAllocator interface {
	AllocateIP(ctx context.Context, region, az, allocType, allocID, eniID, instanceID string) (publicIP, poolName string, err error)
}
