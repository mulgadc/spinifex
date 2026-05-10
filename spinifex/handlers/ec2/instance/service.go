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
}

// ResourceCapacityProvider exposes per-node instance-type availability used by
// DescribeInstanceTypes. Implemented by daemon.ResourceManager.
type ResourceCapacityProvider interface {
	GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo
}

// StoppedInstanceStore provides KV-backed read access to stopped/terminated
// instances. Implemented by daemon.JetStreamManager.
type StoppedInstanceStore interface {
	LoadStoppedInstance(instanceID string) (*vm.VM, error)
	ListStoppedInstances() ([]*vm.VM, error)
	ListTerminatedInstances() ([]*vm.VM, error)
}
