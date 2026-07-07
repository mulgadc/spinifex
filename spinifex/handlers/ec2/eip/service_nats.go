package handlers_ec2_eip

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure NATSEIPService implements EIPService.
var _ EIPService = (*NATSEIPService)(nil)

// NATSEIPService handles Elastic IP operations via NATS messaging.
type NATSEIPService struct {
	natsConn *nats.Conn
}

// NewNATSEIPService creates a new NATS-based EIP service.
func NewNATSEIPService(conn *nats.Conn) EIPService {
	return &NATSEIPService{natsConn: conn}
}

func (s *NATSEIPService) AllocateAddress(ctx context.Context, input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error) {
	return utils.NATSRequestCtx[ec2.AllocateAddressOutput](ctx, s.natsConn, "ec2.AllocateAddress", input, 30*time.Second, accountID)
}

func (s *NATSEIPService) ReleaseAddress(ctx context.Context, input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error) {
	return utils.NATSRequestCtx[ec2.ReleaseAddressOutput](ctx, s.natsConn, "ec2.ReleaseAddress", input, 30*time.Second, accountID)
}

func (s *NATSEIPService) AssociateAddress(ctx context.Context, input *ec2.AssociateAddressInput, accountID string) (*ec2.AssociateAddressOutput, error) {
	return utils.NATSRequestCtx[ec2.AssociateAddressOutput](ctx, s.natsConn, "ec2.AssociateAddress", input, 30*time.Second, accountID)
}

func (s *NATSEIPService) DisassociateAddress(ctx context.Context, input *ec2.DisassociateAddressInput, accountID string) (*ec2.DisassociateAddressOutput, error) {
	return utils.NATSRequestCtx[ec2.DisassociateAddressOutput](ctx, s.natsConn, "ec2.DisassociateAddress", input, 30*time.Second, accountID)
}

func (s *NATSEIPService) DescribeAddresses(ctx context.Context, input *ec2.DescribeAddressesInput, accountID string) (*ec2.DescribeAddressesOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeAddressesOutput](ctx, s.natsConn, "ec2.DescribeAddresses", input, 30*time.Second, accountID)
}

func (s *NATSEIPService) DescribeAddressesAttribute(ctx context.Context, input *ec2.DescribeAddressesAttributeInput, accountID string) (*ec2.DescribeAddressesAttributeOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeAddressesAttributeOutput](ctx, s.natsConn, "ec2.DescribeAddressesAttribute", input, 30*time.Second, accountID)
}
