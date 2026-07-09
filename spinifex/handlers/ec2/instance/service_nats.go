package handlers_ec2_instance

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSInstanceService handles instance operations via NATS. Fan-out methods
// (DescribeInstances, DescribeInstanceTypes) intentionally error — use the
// gateway's scatter-gather helper for cluster-wide results.
type NATSInstanceService struct {
	natsConn *nats.Conn
}

var _ InstanceService = (*NATSInstanceService)(nil)

// NewNATSInstanceService creates a new NATS-based instance service
func NewNATSInstanceService(conn *nats.Conn) InstanceService {
	return &NATSInstanceService{natsConn: conn}
}

func (s *NATSInstanceService) RunInstances(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	if input == nil || input.InstanceType == nil {
		return nil, fmt.Errorf("instance type is required")
	}
	topic := fmt.Sprintf("ec2.RunInstances.%s", aws.StringValue(input.InstanceType))
	return utils.NatsRequest[ec2.Reservation](ctx, s.natsConn, topic, input, 5*time.Minute, accountID)
}

// DescribeInstances is a fan-out operation; use the gateway's scatter-gather
// helper instead of this single-node client.
func (s *NATSInstanceService) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return nil, fmt.Errorf("DescribeInstances is fan-out; use gateway scatter-gather, not NATSInstanceService")
}

// DescribeInstanceTypes is a fan-out operation; use the gateway's scatter-gather
// helper instead of this single-node client.
func (s *NATSInstanceService) DescribeInstanceTypes(_ context.Context, _ *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	return nil, fmt.Errorf("DescribeInstanceTypes is fan-out; use gateway scatter-gather, not NATSInstanceService")
}

// DescribeInstanceStatus is a fan-out operation; use the gateway's scatter-gather
// helper instead of this single-node client.
func (s *NATSInstanceService) DescribeInstanceStatus(_ context.Context, _ *ec2.DescribeInstanceStatusInput, _ string) (*ec2.DescribeInstanceStatusOutput, error) {
	return nil, fmt.Errorf("DescribeInstanceStatus is fan-out; use gateway scatter-gather, not NATSInstanceService")
}

func (s *NATSInstanceService) DescribeInstanceAttribute(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	return utils.NatsRequest[ec2.DescribeInstanceAttributeOutput](ctx, s.natsConn, "ec2.DescribeInstanceAttribute", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) DescribeStoppedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return utils.NatsRequest[ec2.DescribeInstancesOutput](ctx, s.natsConn, "ec2.DescribeStoppedInstances", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) DescribeTerminatedInstances(ctx context.Context, input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return utils.NatsRequest[ec2.DescribeInstancesOutput](ctx, s.natsConn, "ec2.DescribeTerminatedInstances", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) ModifyInstanceAttribute(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, accountID string) (*ec2.ModifyInstanceAttributeOutput, error) {
	return utils.NatsRequest[ec2.ModifyInstanceAttributeOutput](ctx, s.natsConn, "ec2.ModifyInstanceAttribute", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) ModifyInstanceMetadataOptions(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, accountID string) (*ec2.ModifyInstanceMetadataOptionsOutput, error) {
	return utils.NatsRequest[ec2.ModifyInstanceMetadataOptionsOutput](ctx, s.natsConn, "ec2.ModifyInstanceMetadataOptions", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) StartStoppedInstance(ctx context.Context, input *StartStoppedInstanceInput, accountID string) (*StartStoppedInstanceOutput, error) {
	return utils.NatsRequest[StartStoppedInstanceOutput](ctx, s.natsConn, "ec2.StartStoppedInstance", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) TerminateStoppedInstance(ctx context.Context, input *TerminateStoppedInstanceInput, accountID string) (*TerminateStoppedInstanceOutput, error) {
	return utils.NatsRequest[TerminateStoppedInstanceOutput](ctx, s.natsConn, "ec2.TerminateStoppedInstance", input, 30*time.Second, accountID)
}
