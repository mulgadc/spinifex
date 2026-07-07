package handlers_ec2_eip

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// EIPService defines the interface for Elastic IP address operations.
type EIPService interface {
	AllocateAddress(ctx context.Context, input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(ctx context.Context, input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error)
	AssociateAddress(ctx context.Context, input *ec2.AssociateAddressInput, accountID string) (*ec2.AssociateAddressOutput, error)
	DisassociateAddress(ctx context.Context, input *ec2.DisassociateAddressInput, accountID string) (*ec2.DisassociateAddressOutput, error)
	DescribeAddresses(ctx context.Context, input *ec2.DescribeAddressesInput, accountID string) (*ec2.DescribeAddressesOutput, error)
	DescribeAddressesAttribute(ctx context.Context, input *ec2.DescribeAddressesAttributeInput, accountID string) (*ec2.DescribeAddressesAttributeOutput, error)
}
