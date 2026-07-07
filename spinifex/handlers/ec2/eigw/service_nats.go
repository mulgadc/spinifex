package handlers_ec2_eigw

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure NATSEgressOnlyIGWService implements EgressOnlyIGWService
var _ EgressOnlyIGWService = (*NATSEgressOnlyIGWService)(nil)

// NATSEgressOnlyIGWService handles Egress-only Internet Gateway operations via NATS messaging
type NATSEgressOnlyIGWService struct {
	natsConn *nats.Conn
}

// NewNATSEgressOnlyIGWService creates a new NATS-based Egress-only Internet Gateway service
func NewNATSEgressOnlyIGWService(conn *nats.Conn) EgressOnlyIGWService {
	return &NATSEgressOnlyIGWService{natsConn: conn}
}

func (s *NATSEgressOnlyIGWService) CreateEgressOnlyInternetGateway(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, accountID string) (*ec2.CreateEgressOnlyInternetGatewayOutput, error) {
	return utils.NATSRequestCtx[ec2.CreateEgressOnlyInternetGatewayOutput](ctx, s.natsConn, "ec2.CreateEgressOnlyInternetGateway", input, 30*time.Second, accountID)
}

func (s *NATSEgressOnlyIGWService) DeleteEgressOnlyInternetGateway(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, accountID string) (*ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	return utils.NATSRequestCtx[ec2.DeleteEgressOnlyInternetGatewayOutput](ctx, s.natsConn, "ec2.DeleteEgressOnlyInternetGateway", input, 30*time.Second, accountID)
}

func (s *NATSEgressOnlyIGWService) DescribeEgressOnlyInternetGateways(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, accountID string) (*ec2.DescribeEgressOnlyInternetGatewaysOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeEgressOnlyInternetGatewaysOutput](ctx, s.natsConn, "ec2.DescribeEgressOnlyInternetGateways", input, 30*time.Second, accountID)
}
