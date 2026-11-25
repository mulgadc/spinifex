// Package tgw implements the EC2 Transit Gateway and VPC attachment actions
// over JetStream KV, the reconciler's confirmations, and the sweep that ages
// out deleted records and removes orphaned keys.
package tgw

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// TransitGatewayService defines the Transit Gateway and VPC attachment actions.
type TransitGatewayService interface {
	CreateTransitGateway(ctx context.Context, input *ec2.CreateTransitGatewayInput, accountID string) (*ec2.CreateTransitGatewayOutput, error)
	DeleteTransitGateway(ctx context.Context, input *ec2.DeleteTransitGatewayInput, accountID string) (*ec2.DeleteTransitGatewayOutput, error)
	DescribeTransitGateways(ctx context.Context, input *ec2.DescribeTransitGatewaysInput, accountID string) (*ec2.DescribeTransitGatewaysOutput, error)
	CreateTransitGatewayVpcAttachment(ctx context.Context, input *ec2.CreateTransitGatewayVpcAttachmentInput, accountID string) (*ec2.CreateTransitGatewayVpcAttachmentOutput, error)
	DeleteTransitGatewayVpcAttachment(ctx context.Context, input *ec2.DeleteTransitGatewayVpcAttachmentInput, accountID string) (*ec2.DeleteTransitGatewayVpcAttachmentOutput, error)
	DescribeTransitGatewayVpcAttachments(ctx context.Context, input *ec2.DescribeTransitGatewayVpcAttachmentsInput, accountID string) (*ec2.DescribeTransitGatewayVpcAttachmentsOutput, error)
}
