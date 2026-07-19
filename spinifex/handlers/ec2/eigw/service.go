package handlers_ec2_eigw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// EgressOnlyIGWService defines the interface for Egress-only Internet Gateway operations.
type EgressOnlyIGWService interface {
	CreateEgressOnlyInternetGateway(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, accountID string) (*ec2.CreateEgressOnlyInternetGatewayOutput, error)
	DeleteEgressOnlyInternetGateway(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, accountID string) (*ec2.DeleteEgressOnlyInternetGatewayOutput, error)
	DescribeEgressOnlyInternetGateways(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, accountID string) (*ec2.DescribeEgressOnlyInternetGatewaysOutput, error)
}
