package handlers_ec2_natgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// NatGatewayService defines the interface for NAT Gateway operations
type NatGatewayService interface {
	CreateNatGateway(ctx context.Context, input *ec2.CreateNatGatewayInput, accountID string) (*ec2.CreateNatGatewayOutput, error)
	DeleteNatGateway(ctx context.Context, input *ec2.DeleteNatGatewayInput, accountID string) (*ec2.DeleteNatGatewayOutput, error)
	DescribeNatGateways(ctx context.Context, input *ec2.DescribeNatGatewaysInput, accountID string) (*ec2.DescribeNatGatewaysOutput, error)
}
