package handlers_ec2_routetable

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure NATSRouteTableService implements RouteTableService
var _ RouteTableService = (*NATSRouteTableService)(nil)

// NATSRouteTableService handles Route Table operations via NATS messaging
type NATSRouteTableService struct {
	natsConn *nats.Conn
}

// NewNATSRouteTableService creates a new NATS-based Route Table service
func NewNATSRouteTableService(conn *nats.Conn) RouteTableService {
	return &NATSRouteTableService{natsConn: conn}
}

func (s *NATSRouteTableService) CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error) {
	return utils.NATSRequestCtx[ec2.CreateRouteTableOutput](ctx, s.natsConn, "ec2.CreateRouteTable", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, accountID string) (*ec2.DeleteRouteTableOutput, error) {
	return utils.NATSRequestCtx[ec2.DeleteRouteTableOutput](ctx, s.natsConn, "ec2.DeleteRouteTable", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, accountID string) (*ec2.DescribeRouteTablesOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeRouteTablesOutput](ctx, s.natsConn, "ec2.DescribeRouteTables", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error) {
	return utils.NATSRequestCtx[ec2.CreateRouteOutput](ctx, s.natsConn, "ec2.CreateRoute", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) DeleteRoute(ctx context.Context, input *ec2.DeleteRouteInput, accountID string) (*ec2.DeleteRouteOutput, error) {
	return utils.NATSRequestCtx[ec2.DeleteRouteOutput](ctx, s.natsConn, "ec2.DeleteRoute", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) ReplaceRoute(ctx context.Context, input *ec2.ReplaceRouteInput, accountID string) (*ec2.ReplaceRouteOutput, error) {
	return utils.NATSRequestCtx[ec2.ReplaceRouteOutput](ctx, s.natsConn, "ec2.ReplaceRoute", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) AssociateRouteTable(ctx context.Context, input *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error) {
	return utils.NATSRequestCtx[ec2.AssociateRouteTableOutput](ctx, s.natsConn, "ec2.AssociateRouteTable", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, accountID string) (*ec2.DisassociateRouteTableOutput, error) {
	return utils.NATSRequestCtx[ec2.DisassociateRouteTableOutput](ctx, s.natsConn, "ec2.DisassociateRouteTable", input, 30*time.Second, accountID)
}

func (s *NATSRouteTableService) ReplaceRouteTableAssociation(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, accountID string) (*ec2.ReplaceRouteTableAssociationOutput, error) {
	return utils.NATSRequestCtx[ec2.ReplaceRouteTableAssociationOutput](ctx, s.natsConn, "ec2.ReplaceRouteTableAssociation", input, 30*time.Second, accountID)
}
