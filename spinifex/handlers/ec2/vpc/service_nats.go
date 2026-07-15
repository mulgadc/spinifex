package handlers_ec2_vpc

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSVPCService handles VPC and Subnet operations via NATS messaging.
type NATSVPCService struct {
	natsConn *nats.Conn
}

var _ VPCService = (*NATSVPCService)(nil)

// NewNATSVPCService creates a new NATS-based VPC service.
func NewNATSVPCService(conn *nats.Conn) VPCService {
	return &NATSVPCService{natsConn: conn}
}

func (s *NATSVPCService) CreateVpc(ctx context.Context, input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error) {
	return utils.NATSRequest[ec2.CreateVpcOutput](ctx, s.natsConn, "ec2.CreateVpc", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteVpc(ctx context.Context, input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error) {
	return utils.NATSRequest[ec2.DeleteVpcOutput](ctx, s.natsConn, "ec2.DeleteVpc", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error) {
	return utils.NATSRequest[ec2.DescribeVpcsOutput](ctx, s.natsConn, "ec2.DescribeVpcs", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateSubnet(ctx context.Context, input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error) {
	return utils.NATSRequest[ec2.CreateSubnetOutput](ctx, s.natsConn, "ec2.CreateSubnet", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteSubnet(ctx context.Context, input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error) {
	return utils.NATSRequest[ec2.DeleteSubnetOutput](ctx, s.natsConn, "ec2.DeleteSubnet", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSubnetsOutput](ctx, s.natsConn, "ec2.DescribeSubnets", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifySubnetAttribute(ctx context.Context, input *ec2.ModifySubnetAttributeInput, accountID string) (*ec2.ModifySubnetAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifySubnetAttributeOutput](ctx, s.natsConn, "ec2.ModifySubnetAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifyVpcAttribute(ctx context.Context, input *ec2.ModifyVpcAttributeInput, accountID string) (*ec2.ModifyVpcAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifyVpcAttributeOutput](ctx, s.natsConn, "ec2.ModifyVpcAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeVpcAttribute(ctx context.Context, input *ec2.DescribeVpcAttributeInput, accountID string) (*ec2.DescribeVpcAttributeOutput, error) {
	return utils.NATSRequest[ec2.DescribeVpcAttributeOutput](ctx, s.natsConn, "ec2.DescribeVpcAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	return utils.NATSRequest[ec2.CreateNetworkInterfaceOutput](ctx, s.natsConn, "ec2.CreateNetworkInterface", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	return utils.NATSRequest[ec2.DeleteNetworkInterfaceOutput](ctx, s.natsConn, "ec2.DeleteNetworkInterface", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return utils.NATSRequest[ec2.DescribeNetworkInterfacesOutput](ctx, s.natsConn, "ec2.DescribeNetworkInterfaces", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, accountID string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifyNetworkInterfaceAttributeOutput](ctx, s.natsConn, "ec2.ModifyNetworkInterfaceAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error) {
	return utils.NATSRequest[ec2.CreateSecurityGroupOutput](ctx, s.natsConn, "ec2.CreateSecurityGroup", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteSecurityGroup(ctx context.Context, input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error) {
	return utils.NATSRequest[ec2.DeleteSecurityGroupOutput](ctx, s.natsConn, "ec2.DeleteSecurityGroup", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSecurityGroupsOutput](ctx, s.natsConn, "ec2.DescribeSecurityGroups", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSecurityGroupRules(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, accountID string) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	return utils.NATSRequest[ec2.DescribeSecurityGroupRulesOutput](ctx, s.natsConn, "ec2.DescribeSecurityGroupRules", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) AuthorizeSecurityGroupIngress(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return utils.NATSRequest[ec2.AuthorizeSecurityGroupIngressOutput](ctx, s.natsConn, "ec2.AuthorizeSecurityGroupIngress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) AuthorizeSecurityGroupEgress(ctx context.Context, input *ec2.AuthorizeSecurityGroupEgressInput, accountID string) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	return utils.NATSRequest[ec2.AuthorizeSecurityGroupEgressOutput](ctx, s.natsConn, "ec2.AuthorizeSecurityGroupEgress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) RevokeSecurityGroupIngress(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return utils.NATSRequest[ec2.RevokeSecurityGroupIngressOutput](ctx, s.natsConn, "ec2.RevokeSecurityGroupIngress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) RevokeSecurityGroupEgress(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	return utils.NATSRequest[ec2.RevokeSecurityGroupEgressOutput](ctx, s.natsConn, "ec2.RevokeSecurityGroupEgress", input, 30*time.Second, accountID)
}
