package handlers_ec2_spotinstance

import (
	"context"
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

func (s *NATSSpotInstanceService) PutSpotInstanceRequests(ctx context.Context, input *PutSpotRequestsInput, accountID string) (*PutSpotRequestsOutput, error) {
	return utils.NATSRequestCtx[PutSpotRequestsOutput](ctx, s.natsConn, "ec2.PutSpotInstanceRequests", input, 30*time.Second, accountID)
}

func (s *NATSSpotInstanceService) DescribeSpotInstanceRequests(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, accountID string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeSpotInstanceRequestsOutput](ctx, s.natsConn, "ec2.DescribeSpotInstanceRequests", input, 30*time.Second, accountID)
}

func (s *NATSSpotInstanceService) CancelSpotInstanceRequests(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	return utils.NATSRequestCtx[ec2.CancelSpotInstanceRequestsOutput](ctx, s.natsConn, "ec2.CancelSpotInstanceRequests", input, 30*time.Second, accountID)
}
