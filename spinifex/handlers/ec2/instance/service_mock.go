package handlers_ec2_instance

import (
	"context"

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

func (s *MockInstanceService) RunInstances(_ context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
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

func (s *MockInstanceService) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (s *MockInstanceService) DescribeInstanceTypes(_ context.Context, _ *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{}, nil
}

func (s *MockInstanceService) DescribeInstanceStatus(_ context.Context, _ *ec2.DescribeInstanceStatusInput, _ string) (*ec2.DescribeInstanceStatusOutput, error) {
	return &ec2.DescribeInstanceStatusOutput{}, nil
}

func (s *MockInstanceService) DescribeInstanceAttribute(_ context.Context, input *ec2.DescribeInstanceAttributeInput, _ string) (*ec2.DescribeInstanceAttributeOutput, error) {
	return &ec2.DescribeInstanceAttributeOutput{InstanceId: input.InstanceId}, nil
}

func (s *MockInstanceService) DescribeStoppedInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (s *MockInstanceService) DescribeTerminatedInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (s *MockInstanceService) ModifyInstanceAttribute(_ context.Context, _ *ec2.ModifyInstanceAttributeInput, _ string) (*ec2.ModifyInstanceAttributeOutput, error) {
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

func (s *MockInstanceService) ModifyInstanceMetadataOptions(_ context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, _ string) (*ec2.ModifyInstanceMetadataOptionsOutput, error) {
	return &ec2.ModifyInstanceMetadataOptionsOutput{InstanceId: input.InstanceId}, nil
}

func (s *MockInstanceService) StartStoppedInstance(_ context.Context, input *StartStoppedInstanceInput, _ string) (*StartStoppedInstanceOutput, error) {
	return &StartStoppedInstanceOutput{Status: "running", InstanceID: input.InstanceID}, nil
}

func (s *MockInstanceService) TerminateStoppedInstance(_ context.Context, input *TerminateStoppedInstanceInput, _ string) (*TerminateStoppedInstanceOutput, error) {
	return &TerminateStoppedInstanceOutput{Status: "terminated", InstanceID: input.InstanceID}, nil
}
