package vpcendpoint

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

var _ VpcEndpointService = (*NATSVpcEndpointService)(nil)

// natsRequestTimeout is the per-request budget the claim grace is sized from.
const natsRequestTimeout = 30 * time.Second

// NATSVpcEndpointService forwards the endpoint actions to the daemon over NATS.
type NATSVpcEndpointService struct {
	natsConn *nats.Conn
}

// NewNATSVpcEndpointService creates the NATS client for the endpoint actions.
func NewNATSVpcEndpointService(conn *nats.Conn) VpcEndpointService {
	return &NATSVpcEndpointService{natsConn: conn}
}

func (s *NATSVpcEndpointService) CreateVpcEndpoint(ctx context.Context, input *ec2.CreateVpcEndpointInput, accountID string) (*ec2.CreateVpcEndpointOutput, error) {
	return natsmsg.NATSRequest[ec2.CreateVpcEndpointOutput](ctx, s.natsConn, "ec2.CreateVpcEndpoint", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcEndpointService) ModifyVpcEndpoint(ctx context.Context, input *ec2.ModifyVpcEndpointInput, accountID string) (*ec2.ModifyVpcEndpointOutput, error) {
	return natsmsg.NATSRequest[ec2.ModifyVpcEndpointOutput](ctx, s.natsConn, "ec2.ModifyVpcEndpoint", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcEndpointService) DeleteVpcEndpoints(ctx context.Context, input *ec2.DeleteVpcEndpointsInput, accountID string) (*ec2.DeleteVpcEndpointsOutput, error) {
	return natsmsg.NATSRequest[ec2.DeleteVpcEndpointsOutput](ctx, s.natsConn, "ec2.DeleteVpcEndpoints", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcEndpointService) DescribeVpcEndpoints(ctx context.Context, input *ec2.DescribeVpcEndpointsInput, accountID string) (*ec2.DescribeVpcEndpointsOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeVpcEndpointsOutput](ctx, s.natsConn, "ec2.DescribeVpcEndpoints", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcEndpointService) DescribeVpcEndpointServices(ctx context.Context, input *ec2.DescribeVpcEndpointServicesInput, accountID string) (*ec2.DescribeVpcEndpointServicesOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeVpcEndpointServicesOutput](ctx, s.natsConn, "ec2.DescribeVpcEndpointServices", input, natsRequestTimeout, accountID)
}
