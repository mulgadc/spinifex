package handlers_ec2_instance

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSInstanceService handles instance operations via NATS messaging.
//
// Note: DescribeInstances and DescribeInstanceTypes subscriptions are
// fan-out (no queue group); a single NATSRequest only captures one
// responder. Cluster-wide aggregation is the gateway's job (see
// spinifex/gateway/ec2/instance). The methods below are kept for symmetry
// with the InstanceService interface and intentionally fail rather than
// silently returning a one-node view.
type NATSInstanceService struct {
	natsConn *nats.Conn
}

var _ InstanceService = (*NATSInstanceService)(nil)

// NewNATSInstanceService creates a new NATS-based instance service
func NewNATSInstanceService(conn *nats.Conn) InstanceService {
	return &NATSInstanceService{natsConn: conn}
}

func (s *NATSInstanceService) RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	if input == nil || input.InstanceType == nil {
		return nil, fmt.Errorf("instance type is required")
	}
	topic := fmt.Sprintf("ec2.RunInstances.%s", aws.StringValue(input.InstanceType))
	return utils.NATSRequest[ec2.Reservation](s.natsConn, topic, input, 5*time.Minute, accountID)
}

// DescribeInstances is a fan-out operation; use the gateway's scatter-gather
// helper instead of this single-node client.
func (s *NATSInstanceService) DescribeInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return nil, fmt.Errorf("DescribeInstances is fan-out; use gateway scatter-gather, not NATSInstanceService")
}

// DescribeInstanceTypes is a fan-out operation; use the gateway's scatter-gather
// helper instead of this single-node client.
func (s *NATSInstanceService) DescribeInstanceTypes(_ *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	return nil, fmt.Errorf("DescribeInstanceTypes is fan-out; use gateway scatter-gather, not NATSInstanceService")
}

func (s *NATSInstanceService) DescribeInstanceAttribute(input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	return utils.NATSRequest[ec2.DescribeInstanceAttributeOutput](s.natsConn, "ec2.DescribeInstanceAttribute", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) DescribeStoppedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return utils.NATSRequest[ec2.DescribeInstancesOutput](s.natsConn, "ec2.DescribeStoppedInstances", input, 30*time.Second, accountID)
}

func (s *NATSInstanceService) DescribeTerminatedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return utils.NATSRequest[ec2.DescribeInstancesOutput](s.natsConn, "ec2.DescribeTerminatedInstances", input, 30*time.Second, accountID)
}
