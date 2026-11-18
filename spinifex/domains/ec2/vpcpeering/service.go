// Package vpcpeering implements the EC2 VPC peering actions over JetStream KV,
// the reconciler's MarkActive and MarkCleaned confirmations, and the sweep
// that retires expired requests and orphaned keys.
package vpcpeering

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// VpcPeeringService defines the EC2 VPC peering connection actions.
type VpcPeeringService interface {
	CreateVpcPeeringConnection(ctx context.Context, input *ec2.CreateVpcPeeringConnectionInput, accountID string) (*ec2.CreateVpcPeeringConnectionOutput, error)
	AcceptVpcPeeringConnection(ctx context.Context, input *ec2.AcceptVpcPeeringConnectionInput, accountID string) (*ec2.AcceptVpcPeeringConnectionOutput, error)
	RejectVpcPeeringConnection(ctx context.Context, input *ec2.RejectVpcPeeringConnectionInput, accountID string) (*ec2.RejectVpcPeeringConnectionOutput, error)
	DeleteVpcPeeringConnection(ctx context.Context, input *ec2.DeleteVpcPeeringConnectionInput, accountID string) (*ec2.DeleteVpcPeeringConnectionOutput, error)
	DescribeVpcPeeringConnections(ctx context.Context, input *ec2.DescribeVpcPeeringConnectionsInput, accountID string) (*ec2.DescribeVpcPeeringConnectionsOutput, error)
	ModifyVpcPeeringConnectionOptions(ctx context.Context, input *ec2.ModifyVpcPeeringConnectionOptionsInput, accountID string) (*ec2.ModifyVpcPeeringConnectionOptionsOutput, error)
}
