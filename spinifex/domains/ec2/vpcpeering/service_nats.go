package vpcpeering

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

var _ VpcPeeringService = (*NATSVpcPeeringService)(nil)

// natsRequestTimeout is the per-request budget the claim grace is sized from.
const natsRequestTimeout = 30 * time.Second

// NATSVpcPeeringService forwards the peering actions to the daemon over NATS.
type NATSVpcPeeringService struct {
	natsConn *nats.Conn
}

// NewNATSVpcPeeringService creates the NATS client for the peering actions.
func NewNATSVpcPeeringService(conn *nats.Conn) VpcPeeringService {
	return &NATSVpcPeeringService{natsConn: conn}
}

func (s *NATSVpcPeeringService) CreateVpcPeeringConnection(ctx context.Context, input *ec2.CreateVpcPeeringConnectionInput, accountID string) (*ec2.CreateVpcPeeringConnectionOutput, error) {
	return natsmsg.NATSRequest[ec2.CreateVpcPeeringConnectionOutput](ctx, s.natsConn, "ec2.CreateVpcPeeringConnection", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcPeeringService) AcceptVpcPeeringConnection(ctx context.Context, input *ec2.AcceptVpcPeeringConnectionInput, accountID string) (*ec2.AcceptVpcPeeringConnectionOutput, error) {
	return natsmsg.NATSRequest[ec2.AcceptVpcPeeringConnectionOutput](ctx, s.natsConn, "ec2.AcceptVpcPeeringConnection", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcPeeringService) RejectVpcPeeringConnection(ctx context.Context, input *ec2.RejectVpcPeeringConnectionInput, accountID string) (*ec2.RejectVpcPeeringConnectionOutput, error) {
	return natsmsg.NATSRequest[ec2.RejectVpcPeeringConnectionOutput](ctx, s.natsConn, "ec2.RejectVpcPeeringConnection", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcPeeringService) DeleteVpcPeeringConnection(ctx context.Context, input *ec2.DeleteVpcPeeringConnectionInput, accountID string) (*ec2.DeleteVpcPeeringConnectionOutput, error) {
	return natsmsg.NATSRequest[ec2.DeleteVpcPeeringConnectionOutput](ctx, s.natsConn, "ec2.DeleteVpcPeeringConnection", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcPeeringService) DescribeVpcPeeringConnections(ctx context.Context, input *ec2.DescribeVpcPeeringConnectionsInput, accountID string) (*ec2.DescribeVpcPeeringConnectionsOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeVpcPeeringConnectionsOutput](ctx, s.natsConn, "ec2.DescribeVpcPeeringConnections", input, natsRequestTimeout, accountID)
}

func (s *NATSVpcPeeringService) ModifyVpcPeeringConnectionOptions(ctx context.Context, input *ec2.ModifyVpcPeeringConnectionOptionsInput, accountID string) (*ec2.ModifyVpcPeeringConnectionOptionsOutput, error) {
	return natsmsg.NATSRequest[ec2.ModifyVpcPeeringConnectionOptionsOutput](ctx, s.natsConn, "ec2.ModifyVpcPeeringConnectionOptions", input, natsRequestTimeout, accountID)
}
