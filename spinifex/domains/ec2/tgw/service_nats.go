package tgw

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

var _ TransitGatewayService = (*NATSTransitGatewayService)(nil)

// natsRequestTimeout is the per-request budget the claim grace is sized from.
const natsRequestTimeout = 30 * time.Second

// NATSTransitGatewayService forwards the Transit Gateway actions to the daemon over NATS.
type NATSTransitGatewayService struct {
	natsConn *nats.Conn
}

// NewNATSTransitGatewayService creates the NATS client for the Transit Gateway actions.
func NewNATSTransitGatewayService(conn *nats.Conn) TransitGatewayService {
	return &NATSTransitGatewayService{natsConn: conn}
}

func (s *NATSTransitGatewayService) CreateTransitGateway(ctx context.Context, input *ec2.CreateTransitGatewayInput, accountID string) (*ec2.CreateTransitGatewayOutput, error) {
	return natsmsg.NATSRequest[ec2.CreateTransitGatewayOutput](ctx, s.natsConn, "ec2.CreateTransitGateway", input, natsRequestTimeout, accountID)
}

func (s *NATSTransitGatewayService) DeleteTransitGateway(ctx context.Context, input *ec2.DeleteTransitGatewayInput, accountID string) (*ec2.DeleteTransitGatewayOutput, error) {
	return natsmsg.NATSRequest[ec2.DeleteTransitGatewayOutput](ctx, s.natsConn, "ec2.DeleteTransitGateway", input, natsRequestTimeout, accountID)
}

func (s *NATSTransitGatewayService) DescribeTransitGateways(ctx context.Context, input *ec2.DescribeTransitGatewaysInput, accountID string) (*ec2.DescribeTransitGatewaysOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeTransitGatewaysOutput](ctx, s.natsConn, "ec2.DescribeTransitGateways", input, natsRequestTimeout, accountID)
}

func (s *NATSTransitGatewayService) CreateTransitGatewayVpcAttachment(ctx context.Context, input *ec2.CreateTransitGatewayVpcAttachmentInput, accountID string) (*ec2.CreateTransitGatewayVpcAttachmentOutput, error) {
	return natsmsg.NATSRequest[ec2.CreateTransitGatewayVpcAttachmentOutput](ctx, s.natsConn, "ec2.CreateTransitGatewayVpcAttachment", input, natsRequestTimeout, accountID)
}

func (s *NATSTransitGatewayService) DeleteTransitGatewayVpcAttachment(ctx context.Context, input *ec2.DeleteTransitGatewayVpcAttachmentInput, accountID string) (*ec2.DeleteTransitGatewayVpcAttachmentOutput, error) {
	return natsmsg.NATSRequest[ec2.DeleteTransitGatewayVpcAttachmentOutput](ctx, s.natsConn, "ec2.DeleteTransitGatewayVpcAttachment", input, natsRequestTimeout, accountID)
}

func (s *NATSTransitGatewayService) DescribeTransitGatewayVpcAttachments(ctx context.Context, input *ec2.DescribeTransitGatewayVpcAttachmentsInput, accountID string) (*ec2.DescribeTransitGatewayVpcAttachmentsOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeTransitGatewayVpcAttachmentsOutput](ctx, s.natsConn, "ec2.DescribeTransitGatewayVpcAttachments", input, natsRequestTimeout, accountID)
}
