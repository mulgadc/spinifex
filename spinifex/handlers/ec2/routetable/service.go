package handlers_ec2_routetable

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// RouteTableService defines the interface for Route Table operations
type RouteTableService interface {
	CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error)
	DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, accountID string) (*ec2.DeleteRouteTableOutput, error)
	DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, accountID string) (*ec2.DescribeRouteTablesOutput, error)
	CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error)
	DeleteRoute(ctx context.Context, input *ec2.DeleteRouteInput, accountID string) (*ec2.DeleteRouteOutput, error)
	ReplaceRoute(ctx context.Context, input *ec2.ReplaceRouteInput, accountID string) (*ec2.ReplaceRouteOutput, error)
	AssociateRouteTable(ctx context.Context, input *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error)
	DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, accountID string) (*ec2.DisassociateRouteTableOutput, error)
	ReplaceRouteTableAssociation(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, accountID string) (*ec2.ReplaceRouteTableAssociationOutput, error)
}
