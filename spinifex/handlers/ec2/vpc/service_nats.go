package handlers_ec2_vpc

import (
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSVPCService handles VPC and Subnet operations via NATS messaging
type NATSVPCService struct {
	natsConn *nats.Conn
}

// NewNATSVPCService creates a new NATS-based VPC service
func NewNATSVPCService(conn *nats.Conn) VPCService {
	return &NATSVPCService{natsConn: conn}
}

func (s *NATSVPCService) CreateVpc(input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error) {
	return utils.NATSRequest[ec2.CreateVpcOutput](s.natsConn, "ec2.CreateVpc", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteVpc(input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error) {
	return utils.NATSRequest[ec2.DeleteVpcOutput](s.natsConn, "ec2.DeleteVpc", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeVpcs(input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error) {
	return utils.NATSRequest[ec2.DescribeVpcsOutput](s.natsConn, "ec2.DescribeVpcs", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateSubnet(input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error) {
	return utils.NATSRequest[ec2.CreateSubnetOutput](s.natsConn, "ec2.CreateSubnet", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteSubnet(input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error) {
	return utils.NATSRequest[ec2.DeleteSubnetOutput](s.natsConn, "ec2.DeleteSubnet", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSubnets(input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSubnetsOutput](s.natsConn, "ec2.DescribeSubnets", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifySubnetAttribute(input *ec2.ModifySubnetAttributeInput, accountID string) (*ec2.ModifySubnetAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifySubnetAttributeOutput](s.natsConn, "ec2.ModifySubnetAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifyVpcAttribute(input *ec2.ModifyVpcAttributeInput, accountID string) (*ec2.ModifyVpcAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifyVpcAttributeOutput](s.natsConn, "ec2.ModifyVpcAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeVpcAttribute(input *ec2.DescribeVpcAttributeInput, accountID string) (*ec2.DescribeVpcAttributeOutput, error) {
	return utils.NATSRequest[ec2.DescribeVpcAttributeOutput](s.natsConn, "ec2.DescribeVpcAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	return utils.NATSRequest[ec2.CreateNetworkInterfaceOutput](s.natsConn, "ec2.CreateNetworkInterface", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	return utils.NATSRequest[ec2.DeleteNetworkInterfaceOutput](s.natsConn, "ec2.DeleteNetworkInterface", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeNetworkInterfaces(input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return utils.NATSRequest[ec2.DescribeNetworkInterfacesOutput](s.natsConn, "ec2.DescribeNetworkInterfaces", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) ModifyNetworkInterfaceAttribute(input *ec2.ModifyNetworkInterfaceAttributeInput, accountID string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifyNetworkInterfaceAttributeOutput](s.natsConn, "ec2.ModifyNetworkInterfaceAttribute", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error) {
	return utils.NATSRequest[ec2.CreateSecurityGroupOutput](s.natsConn, "ec2.CreateSecurityGroup", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error) {
	return utils.NATSRequest[ec2.DeleteSecurityGroupOutput](s.natsConn, "ec2.DeleteSecurityGroup", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSecurityGroupsOutput](s.natsConn, "ec2.DescribeSecurityGroups", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) DescribeSecurityGroupRules(input *ec2.DescribeSecurityGroupRulesInput, accountID string) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	return utils.NATSRequest[ec2.DescribeSecurityGroupRulesOutput](s.natsConn, "ec2.DescribeSecurityGroupRules", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) AuthorizeSecurityGroupIngress(input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return utils.NATSRequest[ec2.AuthorizeSecurityGroupIngressOutput](s.natsConn, "ec2.AuthorizeSecurityGroupIngress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) AuthorizeSecurityGroupEgress(input *ec2.AuthorizeSecurityGroupEgressInput, accountID string) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	return utils.NATSRequest[ec2.AuthorizeSecurityGroupEgressOutput](s.natsConn, "ec2.AuthorizeSecurityGroupEgress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) RevokeSecurityGroupIngress(input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return utils.NATSRequest[ec2.RevokeSecurityGroupIngressOutput](s.natsConn, "ec2.RevokeSecurityGroupIngress", input, 30*time.Second, accountID)
}

func (s *NATSVPCService) RevokeSecurityGroupEgress(input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	return utils.NATSRequest[ec2.RevokeSecurityGroupEgressOutput](s.natsConn, "ec2.RevokeSecurityGroupEgress", input, 30*time.Second, accountID)
}
