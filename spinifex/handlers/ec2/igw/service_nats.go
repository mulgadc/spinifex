package handlers_ec2_igw

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure NATSIGWService implements IGWService.
var _ IGWService = (*NATSIGWService)(nil)

// NATSIGWService handles Internet Gateway operations via NATS messaging.
type NATSIGWService struct {
	natsConn *nats.Conn
}

// NewNATSIGWService creates a new NATS-based Internet Gateway service.
func NewNATSIGWService(conn *nats.Conn) IGWService {
	return &NATSIGWService{natsConn: conn}
}

func (s *NATSIGWService) CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error) {
	return utils.NATSRequest[ec2.CreateInternetGatewayOutput](ctx, s.natsConn, "ec2.CreateInternetGateway", input, 30*time.Second, accountID)
}

func (s *NATSIGWService) DeleteInternetGateway(ctx context.Context, input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error) {
	return utils.NATSRequest[ec2.DeleteInternetGatewayOutput](ctx, s.natsConn, "ec2.DeleteInternetGateway", input, 30*time.Second, accountID)
}

func (s *NATSIGWService) DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error) {
	return utils.NATSRequest[ec2.DescribeInternetGatewaysOutput](ctx, s.natsConn, "ec2.DescribeInternetGateways", input, 30*time.Second, accountID)
}

func (s *NATSIGWService) AttachInternetGateway(ctx context.Context, input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error) {
	return utils.NATSRequest[ec2.AttachInternetGatewayOutput](ctx, s.natsConn, "ec2.AttachInternetGateway", input, 30*time.Second, accountID)
}

func (s *NATSIGWService) DetachInternetGateway(ctx context.Context, input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error) {
	return utils.NATSRequest[ec2.DetachInternetGatewayOutput](ctx, s.natsConn, "ec2.DetachInternetGateway", input, 30*time.Second, accountID)
}
