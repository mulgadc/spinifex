// Package vpcendpoint implements the EC2 gateway VPC endpoint actions over
// JetStream KV, the derived route-table associations they publish, and the
// sweep that removes what an interrupted request left behind.
package vpcendpoint

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// VpcEndpointService defines the EC2 VPC endpoint actions.
type VpcEndpointService interface {
	CreateVpcEndpoint(ctx context.Context, input *ec2.CreateVpcEndpointInput, accountID string) (*ec2.CreateVpcEndpointOutput, error)
	ModifyVpcEndpoint(ctx context.Context, input *ec2.ModifyVpcEndpointInput, accountID string) (*ec2.ModifyVpcEndpointOutput, error)
	DeleteVpcEndpoints(ctx context.Context, input *ec2.DeleteVpcEndpointsInput, accountID string) (*ec2.DeleteVpcEndpointsOutput, error)
	DescribeVpcEndpoints(ctx context.Context, input *ec2.DescribeVpcEndpointsInput, accountID string) (*ec2.DescribeVpcEndpointsOutput, error)
	DescribeVpcEndpointServices(ctx context.Context, input *ec2.DescribeVpcEndpointServicesInput, accountID string) (*ec2.DescribeVpcEndpointServicesOutput, error)
}
