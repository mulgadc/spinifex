package handlers_ec2_instance

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// MockInstanceService provides mock responses for testing
type MockInstanceService struct{}

var _ InstanceService = (*MockInstanceService)(nil)

// NewMockInstanceService creates a new mock instance service
func NewMockInstanceService() InstanceService {
	return &MockInstanceService{}
}

func (s *MockInstanceService) RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	instance := &ec2.Instance{
		InstanceId: aws.String("i-0123456789abcdef0"),
		State: &ec2.InstanceState{
			Code: aws.Int64(16),
			Name: aws.String("running"),
		},
		ImageId:      input.ImageId,
		InstanceType: input.InstanceType,
		KeyName:      input.KeyName,
		SubnetId:     input.SubnetId,
	}

	reservation := &ec2.Reservation{
		Instances: []*ec2.Instance{instance},
		OwnerId:   aws.String(accountID),
	}

	return reservation, nil
}

func (s *MockInstanceService) DescribeInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (s *MockInstanceService) DescribeInstanceTypes(_ *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{}, nil
}

func (s *MockInstanceService) DescribeInstanceAttribute(input *ec2.DescribeInstanceAttributeInput, _ string) (*ec2.DescribeInstanceAttributeOutput, error) {
	return &ec2.DescribeInstanceAttributeOutput{InstanceId: input.InstanceId}, nil
}

func (s *MockInstanceService) DescribeStoppedInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (s *MockInstanceService) DescribeTerminatedInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}
