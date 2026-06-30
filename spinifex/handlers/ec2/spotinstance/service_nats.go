package handlers_ec2_spotinstance

import (
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure NATSSpotInstanceService implements SpotInstanceService.
var _ SpotInstanceService = (*NATSSpotInstanceService)(nil)

// NATSSpotInstanceService handles spot instance request operations via NATS messaging.
type NATSSpotInstanceService struct {
	natsConn *nats.Conn
}

// NewNATSSpotInstanceService creates a new NATS-based spot instance service.
func NewNATSSpotInstanceService(conn *nats.Conn) *NATSSpotInstanceService {
	return &NATSSpotInstanceService{natsConn: conn}
}

func (s *NATSSpotInstanceService) PutSpotInstanceRequests(input *PutSpotRequestsInput, accountID string) (*PutSpotRequestsOutput, error) {
	return utils.NATSRequest[PutSpotRequestsOutput](s.natsConn, "ec2.PutSpotInstanceRequests", input, 30*time.Second, accountID)
}

func (s *NATSSpotInstanceService) DescribeSpotInstanceRequests(input *ec2.DescribeSpotInstanceRequestsInput, accountID string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSpotInstanceRequestsOutput](s.natsConn, "ec2.DescribeSpotInstanceRequests", input, 30*time.Second, accountID)
}

func (s *NATSSpotInstanceService) CancelSpotInstanceRequests(input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	return utils.NATSRequest[ec2.CancelSpotInstanceRequestsOutput](s.natsConn, "ec2.CancelSpotInstanceRequests", input, 30*time.Second, accountID)
}
