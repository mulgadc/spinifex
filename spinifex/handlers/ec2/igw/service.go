package handlers_ec2_igw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// IGWService defines the interface for Internet Gateway operations.
type IGWService interface {
	CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error)
	DeleteInternetGateway(ctx context.Context, input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error)
	DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error)
	AttachInternetGateway(ctx context.Context, input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error)
	DetachInternetGateway(ctx context.Context, input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error)
}
