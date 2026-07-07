package handlers_ec2_vpc

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// VPCService defines the interface for VPC, Subnet, ENI, and Security Group operations
type VPCService interface {
	CreateVpc(ctx context.Context, input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error)
	DeleteVpc(ctx context.Context, input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error)
	DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error)
	CreateSubnet(ctx context.Context, input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error)
	DeleteSubnet(ctx context.Context, input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
	ModifySubnetAttribute(ctx context.Context, input *ec2.ModifySubnetAttributeInput, accountID string) (*ec2.ModifySubnetAttributeOutput, error)
	ModifyVpcAttribute(ctx context.Context, input *ec2.ModifyVpcAttributeInput, accountID string) (*ec2.ModifyVpcAttributeOutput, error)
	DescribeVpcAttribute(ctx context.Context, input *ec2.DescribeVpcAttributeInput, accountID string) (*ec2.DescribeVpcAttributeOutput, error)
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
	ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, accountID string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
	CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DeleteSecurityGroup(ctx context.Context, input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error)
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeSecurityGroupRules(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, accountID string) (*ec2.DescribeSecurityGroupRulesOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	AuthorizeSecurityGroupEgress(ctx context.Context, input *ec2.AuthorizeSecurityGroupEgressInput, accountID string) (*ec2.AuthorizeSecurityGroupEgressOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error)
}
