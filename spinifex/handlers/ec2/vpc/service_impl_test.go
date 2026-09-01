package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestVPCServiceWithNC(t *testing.T) (*VPCServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	svc, err := NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)
	return svc, nc
}

func setupTestVPCService(t *testing.T) *VPCServiceImpl {
	t.Helper()
	svc, _ := setupTestVPCServiceWithNC(t)
	return svc
}

// setupTestVPCServiceWithFailingVpcd creates a VPC service whose vpcd stub
// always returns success=false. Used to assert vpcd-side errors surface to the
// API caller.
func setupTestVPCServiceWithFailingVpcd(t *testing.T, errMsg string) (*VPCServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGFailingResponder(t, nc, errMsg)
	return svc, nc
}

func createTestVPC(t *testing.T, svc *VPCServiceImpl, cidr string) string {
	t.Helper()
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String(cidr),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Vpc.VpcId
}

func createTestSubnet(t *testing.T, svc *VPCServiceImpl, vpcID, cidr string) string {
	t.Helper()
	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String(cidr),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Subnet.SubnetId
}

// --- VPC Tests ---

func TestCreateVpc(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Vpc)
	assert.Equal(t, "vpc-", (*out.Vpc.VpcId)[:4])
	assert.Equal(t, "10.0.0.0/16", *out.Vpc.CidrBlock)
	assert.Equal(t, "available", *out.Vpc.State)
	assert.False(t, *out.Vpc.IsDefault)
}

func TestCreateVpc_MissingCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateVpc_EmptyCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String(""),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateVpc_InvalidCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("not-a-cidr"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_CidrTooLarge(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/8"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_CidrTooSmall(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/29"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_WithTags(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-vpc")},
					{Key: aws.String("Env"), Value: aws.String("test")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpc.Tags, 2)

	// Verify tags persist through describe
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds: []*string{out.Vpc.VpcId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.Len(t, desc.Vpcs[0].Tags, 2)
}

func TestCreateVpc_TagsWrongResourceType(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("wrong-type")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpc.Tags)
}

func TestCreateVpc_VNIIncrement(t *testing.T) {
	svc := setupTestVPCService(t)

	// Create two VPCs and verify they get different VNIs
	out1, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	out2, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.1.0.0/16")}, testAccountID)
	require.NoError(t, err)

	// Verify VPCs are different
	assert.NotEqual(t, *out1.Vpc.VpcId, *out2.Vpc.VpcId)
}

func TestDeleteVpc(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
	assert.Nil(t, desc)
}

func TestDeleteVpc_MissingID(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDeleteVpc_WithSubnets(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Should fail because VPC has subnets
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")
	// The message must name the blocking subnet so the caller knows what to
	// delete first, not just that something is blocking.
	assert.ErrorContains(t, err, subnetID)
}

func TestDescribeVpcs_All(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "10.1.0.0/16")

	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 2)
}

func TestDescribeVpcs_ByID(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "10.1.0.0/16")

	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.Equal(t, vpcID, *desc.Vpcs[0].VpcId)
}

func TestDescribeVpcs_Empty(t *testing.T) {
	svc := setupTestVPCService(t)
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Vpcs)
}

func TestDescribeVpcs_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String("vpc-nonexistent")},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

// --- Subnet Tests ---

func TestCreateSubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Subnet)
	assert.Equal(t, "subnet-", (*out.Subnet.SubnetId)[:7])
	assert.Equal(t, vpcID, *out.Subnet.VpcId)
	assert.Equal(t, "10.0.1.0/24", *out.Subnet.CidrBlock)
	assert.Equal(t, "available", *out.Subnet.State)
	// /24 = 256 - 5 reserved = 251
	assert.Equal(t, int64(251), *out.Subnet.AvailableIpAddressCount)
}

func TestCreateSubnet_MissingVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateSubnet_MissingCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateSubnet_InvalidVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String("vpc-nonexistent"),
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

func TestCreateSubnet_InvalidCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("not-a-cidr"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Range")
}

func TestCreateSubnet_OutsideVpcCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("192.168.1.0/24"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Range")
}

func TestCreateSubnet_ConflictingCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Try to create overlapping subnet
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/25"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Conflict")
}

func TestCreateSubnet_WithTags(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/24"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("subnet"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-subnet")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnet.Tags, 1)
}

func TestCreateSubnet_WithAZ(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "us-east-1a", *out.Subnet.AvailabilityZone)
}

func TestDeleteSubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	_, err := svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
	assert.Nil(t, desc)
}

func TestDeleteSubnet_MissingID(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDescribeSubnets_All(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Subnets, 2)
}

func TestDescribeSubnets_ByID(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Subnets, 1)
	assert.Equal(t, subnetID, *desc.Subnets[0].SubnetId)
}

func TestDescribeSubnets_ByVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "10.1.0.0/16")
	createTestSubnet(t, svc, vpc1, "10.0.1.0/24")
	createTestSubnet(t, svc, vpc2, "10.1.1.0/24")

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpc1)},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Subnets, 1)
	assert.Equal(t, vpc1, *desc.Subnets[0].VpcId)
}

func TestDescribeSubnets_Empty(t *testing.T) {
	svc := setupTestVPCService(t)
	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Subnets)
}

func TestDescribeSubnets_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String("subnet-nonexistent")},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
}

func TestCreateMultipleSubnetsInVpc(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Create non-overlapping subnets
	sub1 := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	sub2 := createTestSubnet(t, svc, vpcID, "10.0.2.0/24")
	sub3 := createTestSubnet(t, svc, vpcID, "10.0.3.0/24")

	assert.NotEqual(t, sub1, sub2)
	assert.NotEqual(t, sub2, sub3)

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Subnets, 3)
}

func TestDeleteVpcAfterSubnetsDeleted(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Can't delete VPC with subnets
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")

	// Delete subnet first
	_, err = svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.NoError(t, err)

	// Now VPC can be deleted
	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

func TestCreateSubnet_CidrRanges(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// /28 (smallest allowed) = 16 IPs - 5 reserved = 11
	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.0.0/28"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(11), *out.Subnet.AvailableIpAddressCount)

	// /16 (largest allowed) = 65536 IPs - 5 reserved = 65531
	vpcID2 := createTestVPC(t, svc, "172.16.0.0/16")
	out2, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID2),
		CidrBlock: aws.String("172.16.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(65531), *out2.Subnet.AvailableIpAddressCount)
}

// --- Default VPC Tests ---

func TestEnsureDefaultVPC(t *testing.T) {
	svc := setupTestVPCService(t)

	info, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.NotEmpty(t, info.VpcId)
	assert.NotEmpty(t, info.SubnetId)
	assert.Equal(t, "172.31.0.0/16", info.Cidr)
	assert.Equal(t, "172.31.0.0/20", info.SubnetCidr)

	// Verify default VPC was created
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.True(t, *desc.Vpcs[0].IsDefault)
	assert.Equal(t, "172.31.0.0/16", *desc.Vpcs[0].CidrBlock)

	// Verify default subnet was created
	subDesc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.True(t, *subDesc.Subnets[0].DefaultForAz)
	assert.Equal(t, "172.31.0.0/20", *subDesc.Subnets[0].CidrBlock)
	assert.Equal(t, *desc.Vpcs[0].VpcId, *subDesc.Subnets[0].VpcId)
}

func TestEnsureDefaultVPC_Idempotent(t *testing.T) {
	svc := setupTestVPCService(t)

	// Call twice — should be idempotent
	_, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	// Should still have exactly 1 VPC and 1 subnet
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 1)

	subDesc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, subDesc.Subnets, 1)
}

func TestEnsureDefaultVPC_SkipsWhenDefaultExists(t *testing.T) {
	svc := setupTestVPCService(t)

	// Create default VPC first
	_, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	// Create a second (non-default) VPC
	createTestVPC(t, svc, "10.0.0.0/16")

	// Calling again should not create another default
	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 2) // 1 default + 1 manual

	// Only 1 should be default
	defaultCount := 0
	for _, vpc := range desc.Vpcs {
		if *vpc.IsDefault {
			defaultCount++
		}
	}
	assert.Equal(t, 1, defaultCount)
}

// TestCreateMainRouteTable_Idempotent asserts a second createMainRouteTable
// call for the same VPC is a no-op. Concurrent calls otherwise create duplicate
// IsMain=true records, corrupting route-table resolution.
func TestCreateMainRouteTable_Idempotent(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.99.0.0/16") // CreateVpc auto-calls createMainRouteTable

	firstID, err := svc.findMainRouteTableID(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, firstID, "CreateVpc should have created a main route table")

	require.NoError(t, svc.createMainRouteTable(context.Background(), testAccountID, vpcID, "10.99.0.0/16"))

	secondID, err := svc.findMainRouteTableID(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	assert.Equal(t, firstID, secondID, "second call must be a no-op")

	mains := countMainRouteTablesForVPC(t, svc, vpcID)
	assert.Equal(t, 1, mains, "exactly one IsMain=true record after duplicate call")
}

// TestDeleteVpc_ReapsMainRouteTable asserts DeleteVpc reclaims the VPC's
// auto-created main route table. DeleteRouteTable refuses to delete a main RT
// (AWS-faithful), so without DeleteVpc reaping it the rtbKV bucket leaks one
// orphaned main RT per deleted VPC.
func TestDeleteVpc_ReapsMainRouteTable(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.77.0.0/16") // auto-creates the main RT

	rtbID, err := svc.findMainRouteTableID(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, rtbID, "CreateVpc should have created a main route table")
	require.Equal(t, 1, countMainRouteTablesForVPC(t, svc, vpcID))

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)

	gone, err := svc.findMainRouteTableID(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	assert.Empty(t, gone, "DeleteVpc must reap the main route table, not leak it")
	assert.Equal(t, 0, countMainRouteTablesForVPC(t, svc, vpcID))
}

// countMainRouteTablesForVPC scans rtbKV directly to surface duplicate-main
// state in tests. Returns the number of IsMain=true records for vpcID.
func countMainRouteTablesForVPC(t *testing.T, svc *VPCServiceImpl, vpcID string) int {
	t.Helper()
	keys, err := svc.rtbKV.Keys(t.Context())
	if err != nil {
		return 0
	}
	n := 0
	for _, key := range keys {
		entry, err := svc.rtbKV.Get(t.Context(), key)
		if err != nil {
			continue
		}
		var rt struct {
			VpcId  string `json:"vpc_id"`
			IsMain bool   `json:"is_main"`
		}
		if err := json.Unmarshal(entry.Value(), &rt); err != nil {
			continue
		}
		if rt.VpcId == vpcID && rt.IsMain {
			n++
		}
	}
	return n
}

// nonMainRTBRecord mirrors routetable.RouteTableRecord (see
// clearRouteTableAssociationsForSubnet) so tests can seed a non-main route
// table with an association directly in rtbKV, bypassing the routetable
// package to avoid a circular import.
type nonMainRTBRecord struct {
	RouteTableId string `json:"route_table_id"`
	VpcId        string `json:"vpc_id"`
	AccountID    string `json:"account_id"`
	IsMain       bool   `json:"is_main"`
	Associations []struct {
		AssociationId string `json:"association_id"`
		SubnetId      string `json:"subnet_id,omitempty"`
		Main          bool   `json:"main"`
	} `json:"associations"`
	Tags      map[string]string `json:"tags"`
	CreatedAt time.Time         `json:"created_at"`
}

// putTestNonMainRouteTable writes a non-main route table associated with
// subnetID directly to rtbKV and returns its ID and association ID.
func putTestNonMainRouteTable(t *testing.T, svc *VPCServiceImpl, accountID, vpcID, subnetID string) (rtbID, assocID string) {
	t.Helper()
	rtbID = "rtb-" + t.Name() + "-" + subnetID
	assocID = "rtbassoc-" + t.Name() + "-" + subnetID
	rec := nonMainRTBRecord{
		RouteTableId: rtbID,
		VpcId:        vpcID,
		AccountID:    accountID,
		IsMain:       false,
		Associations: []struct {
			AssociationId string `json:"association_id"`
			SubnetId      string `json:"subnet_id,omitempty"`
			Main          bool   `json:"main"`
		}{
			{AssociationId: assocID, SubnetId: subnetID, Main: false},
		},
		Tags:      make(map[string]string),
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = svc.rtbKV.Put(t.Context(), accountID+"."+rtbID, data)
	require.NoError(t, err)
	return rtbID, assocID
}

// getTestRouteTableAssociations reads back the associations of rtbID for
// assertions.
func getTestRouteTableAssociations(t *testing.T, svc *VPCServiceImpl, accountID, rtbID string) []string {
	t.Helper()
	entry, err := svc.rtbKV.Get(t.Context(), accountID+"."+rtbID)
	require.NoError(t, err)
	var rec nonMainRTBRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	subnetIDs := make([]string, 0, len(rec.Associations))
	for _, a := range rec.Associations {
		subnetIDs = append(subnetIDs, a.SubnetId)
	}
	return subnetIDs
}

// TestDeleteVpc_RejectsNonMainRouteTable asserts DeleteVpc rejects with
// DependencyViolation while a non-main route table exists, matching AWS:
// non-main route tables must be deleted first. Without this check the VPC
// is removed out from under the route table, orphaning it permanently since
// DeleteRouteTable can never resolve a VpcId that no longer exists.
func TestDeleteVpc_RejectsNonMainRouteTable(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.60.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.60.1.0/24")
	rtbID, _ := putTestNonMainRouteTable(t, svc, testAccountID, vpcID, subnetID)

	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.Error(t, err)
	assert.ErrorContains(t, err, "DependencyViolation")

	// Rejected call must mutate nothing: VPC and route table both persist.
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(vpcID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1, "VPC must persist so the caller can delete the route table and retry")

	_, err = svc.rtbKV.Get(t.Context(), testAccountID+"."+rtbID)
	require.NoError(t, err, "non-main route table must not be reaped by a rejected DeleteVpc")
}

// An attach awaiting confirmation is hidden from describes, so the gateway
// record is the only place the attachment is visible. Deleting the VPC anyway
// strips the only id that can detach it, leaving a gateway that can never be
// detached or deleted and an account teardown that can never finish.
func TestDeleteVpc_RejectsAttachedInternetGateway(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.64.0.0/16")

	_, err := svc.igwKV.Put(t.Context(), testAccountID+".igw-pending",
		[]byte(`{"internet_gateway_id":"igw-pending","vpc_id":"`+vpcID+`","state":"available","attach_state":"pending"}`))
	require.NoError(t, err)

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.Error(t, err)
	assert.ErrorContains(t, err, "DependencyViolation")
	assert.ErrorContains(t, err, "igw-pending", "the caller must be told which gateway to detach")

	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(vpcID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1, "VPC must persist so the caller can detach the gateway and retry")
}

// A gateway attached to some other VPC must not block this one.
func TestDeleteVpc_IgnoresInternetGatewayOnAnotherVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.65.0.0/16")

	_, err := svc.igwKV.Put(t.Context(), testAccountID+".igw-elsewhere",
		[]byte(`{"internet_gateway_id":"igw-elsewhere","vpc_id":"vpc-somewhere-else","state":"available"}`))
	require.NoError(t, err)

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

// TestDeleteVpc_RouteTableCheckFailsClosedOnCorruptRTB asserts a corrupt
// route table record blocks DeleteVpc rather than being silently skipped —
// a transient/corrupt read must never let DeleteVpc orphan a route table it
// could not evaluate.
func TestDeleteVpc_RouteTableCheckFailsClosedOnCorruptRTB(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.61.0.0/16")

	_, err := svc.rtbKV.Put(t.Context(), testAccountID+".rtb-corrupt", []byte("{not json"))
	require.NoError(t, err)

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.Error(t, err)
	assert.ErrorContains(t, err, "ServerInternal")
}

// TestDeleteSubnet_ClearsRouteTableAssociations asserts DeleteSubnet removes
// every association naming the deleted subnet, so the route table it leaves
// behind never carries a reference to a subnet that no longer exists.
func TestDeleteSubnet_ClearsRouteTableAssociations(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.62.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.62.1.0/24")
	rtbID, _ := putTestNonMainRouteTable(t, svc, testAccountID, vpcID, subnetID)

	_, err := svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.NoError(t, err)

	assert.NotContains(t, getTestRouteTableAssociations(t, svc, testAccountID, rtbID), subnetID,
		"DeleteSubnet must clear associations naming the deleted subnet")
}

// TestDeleteSubnet_PreservesUnknownRouteTableFields pins the write-back to
// editing only "associations". routetable owns this record and imports vpc, so
// it cannot be typed here; a mirror struct would silently erase every field
// added to RouteTableRecord that the mirror had not caught up with.
func TestDeleteSubnet_PreservesUnknownRouteTableFields(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.63.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.63.1.0/24")
	rtbID, _ := putTestNonMainRouteTable(t, svc, testAccountID, vpcID, subnetID)

	// Re-write the stored record with a field no struct in this package knows.
	key := testAccountID + "." + rtbID
	entry, err := svc.rtbKV.Get(t.Context(), key)
	require.NoError(t, err)
	var stored map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(entry.Value(), &stored))
	stored["propagating_vgws"] = json.RawMessage(`["vgw-deadbeef"]`)
	data, err := json.Marshal(stored)
	require.NoError(t, err)
	_, err = svc.rtbKV.Put(t.Context(), key, data)
	require.NoError(t, err)

	_, err = svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.NoError(t, err)

	after, err := svc.rtbKV.Get(t.Context(), key)
	require.NoError(t, err)
	var reread map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(after.Value(), &reread))

	assert.NotContains(t, getTestRouteTableAssociations(t, svc, testAccountID, rtbID), subnetID,
		"the association naming the subnet must still be cleared")
	assert.JSONEq(t, `["vgw-deadbeef"]`, string(reread["propagating_vgws"]),
		"a field this package does not model must survive the association clear")
}

// TestDeleteSubnet_LeavesOtherVPCsAssociationsAlone asserts clearing
// associations for one subnet does not touch a route table association
// belonging to a different subnet/VPC.
func TestDeleteSubnet_LeavesOtherVPCsAssociationsAlone(t *testing.T) {
	svc := setupTestVPCService(t)

	vpcA := createTestVPC(t, svc, "10.63.0.0/16")
	subnetA := createTestSubnet(t, svc, vpcA, "10.63.1.0/24")
	rtbA, _ := putTestNonMainRouteTable(t, svc, testAccountID, vpcA, subnetA)

	vpcB := createTestVPC(t, svc, "10.64.0.0/16")
	subnetB := createTestSubnet(t, svc, vpcB, "10.64.1.0/24")
	rtbB, _ := putTestNonMainRouteTable(t, svc, testAccountID, vpcB, subnetB)

	_, err := svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetA)}, testAccountID)
	require.NoError(t, err)

	assert.NotContains(t, getTestRouteTableAssociations(t, svc, testAccountID, rtbA), subnetA)
	assert.Contains(t, getTestRouteTableAssociations(t, svc, testAccountID, rtbB), subnetB,
		"deleting a subnet in one VPC must not touch another VPC's route table associations")
}

// TestDeleteSubnet_RouteTableCheckFailsClosedOnCorruptRTB asserts a corrupt
// route table record blocks DeleteSubnet, leaving the subnet present and the
// operation retryable, rather than deleting the subnet with a stale
// association strand behind it unseen.
func TestDeleteSubnet_RouteTableCheckFailsClosedOnCorruptRTB(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.65.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.65.1.0/24")

	_, err := svc.rtbKV.Put(t.Context(), testAccountID+".rtb-corrupt", []byte("{not json"))
	require.NoError(t, err)

	_, err = svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.Error(t, err)
	assert.ErrorContains(t, err, "ServerInternal")

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{SubnetIds: []*string{aws.String(subnetID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Subnets, 1, "subnet must persist so a KV read failure never strands an association unseen")
}

// TestEnsureDefaultVPC_NoVpcdResponder simulates the daemon-startup race where
// EnsureDefaultVPC runs before vpcd has subscribed. The SG step is best-effort,
// so subnet and RTB must still land in KV when vpcd is absent.
func TestEnsureDefaultVPC_NoVpcdResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	// Intentionally NOT calling StubVpcdSGResponder — vpc.create-sg has no
	// responder, mirroring the bootstrap race.

	info, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	require.NotNil(t, info)

	// Default VPC, subnet, and main RTB must all be present in KV.
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.True(t, *desc.Vpcs[0].IsDefault)

	subDesc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.Equal(t, info.VpcId, *subDesc.Subnets[0].VpcId)

	require.NotNil(t, svc.rtbKV)
	rtbKeys, err := svc.rtbKV.Keys(t.Context())
	require.NoError(t, err)
	foundMainRTB := false
	for _, k := range rtbKeys {
		entry, err := svc.rtbKV.Get(t.Context(), k)
		if err != nil {
			continue
		}
		var rec struct {
			VpcId  string `json:"vpc_id"`
			IsMain bool   `json:"is_main"`
		}
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		if rec.VpcId == info.VpcId && rec.IsMain {
			foundMainRTB = true
			break
		}
	}
	assert.True(t, foundMainRTB, "main route table must exist for default VPC even when vpcd is unavailable")

	// Default SG record is best-effort; KV write happens before the synchronous
	// vpcd round-trip, so the record should still be present.
	sgKeys, err := svc.sgKV.Keys(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, sgKeys, "default SG record must persist in KV for vpcd reconciler to converge")
}

func TestGetDefaultSubnet(t *testing.T) {
	svc := setupTestVPCService(t)

	// No default subnet yet
	_, err := svc.GetDefaultSubnet(testAccountID)
	assert.Error(t, err)

	// Create default VPC + subnet
	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	subnet, err := svc.GetDefaultSubnet(testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "172.31.0.0/20", subnet.CidrBlock)
	assert.True(t, subnet.IsDefault)
}

func TestGetDefaultSubnet_NotConfusedByNonDefault(t *testing.T) {
	svc := setupTestVPCService(t)

	// Create a non-default VPC + subnet
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// GetDefaultSubnet should not return the non-default subnet
	_, err := svc.GetDefaultSubnet(testAccountID)
	assert.Error(t, err)

	// Now create default
	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	subnet, err := svc.GetDefaultSubnet(testAccountID)
	require.NoError(t, err)
	assert.True(t, subnet.IsDefault)
}

func TestCreateSubnet_CidrTooSmall(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.0.0/29"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Range")
}

func TestVpcCidrBlockAssociation(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.CidrBlockAssociationSet, 1)
	assert.Equal(t, "10.0.0.0/16", *out.Vpc.CidrBlockAssociationSet[0].CidrBlock)
	assert.Equal(t, "associated", *out.Vpc.CidrBlockAssociationSet[0].CidrBlockState.State)
}

// --- Filter tests ---

func TestDescribeVpcs_NilFields(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	// DescribeVpcs with nil VpcIds and nil Filters should return all
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds:  nil,
		Filters: nil,
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 1)
}

func TestDescribeSubnets_FilterByVpcId_NoMatch(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Filter by a VPC ID that doesn't match any subnet
	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String("vpc-nonexistent")},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Subnets)
}

// --- NATS event tests ---

func TestCreateVpc_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.create", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	vpcID := *out.Vpc.VpcId

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), vpcID)
		assert.Contains(t, string(msg.Data), "10.0.0.0/16")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create event")
	}
}

func TestDeleteVpc_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.delete", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), vpcID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.delete event")
	}
}

func TestCreateSubnet_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.create-subnet", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	subnetID := *out.Subnet.SubnetId

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), subnetID)
		assert.Contains(t, string(msg.Data), vpcID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create-subnet event")
	}
}

func TestDeleteSubnet_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.delete-subnet", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), subnetID)
		assert.Contains(t, string(msg.Data), vpcID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.delete-subnet event")
	}
}

// --- Additional coverage tests ---

func TestEnsureDefaultVPC_WithConfigAZ(t *testing.T) {
	// Create a service with custom config that has AZ set
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	cfg := &config.Config{AZ: "us-west-2b"}
	svc, err := NewVPCServiceImplWithNATS(t.Context(), cfg, nc)
	require.NoError(t, err)

	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	// Verify the subnet uses the configured AZ
	subDesc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.Equal(t, "us-west-2b", *subDesc.Subnets[0].AvailabilityZone)
}

func TestCreateVpc_NormalizesNetworkCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	// Pass a CIDR with host bits set — should be normalized to network address
	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.5/16"),
	}, testAccountID)
	require.NoError(t, err)
	// Should normalize to 10.0.0.0/16
	assert.Equal(t, "10.0.0.0/16", *out.Vpc.CidrBlock)
}

func TestDeleteVpc_WithENIs(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Create an ENI in the subnet
	_, err := svc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	// Delete subnet should succeed (ENI is in subnet but delete checks subnet dependencies in vpc delete)
	// First delete subnet
	_, err = svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	// DeleteSubnet doesn't check for ENIs currently - just deletes
	require.NoError(t, err)

	// Now delete VPC should succeed since subnet is gone
	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

func TestCreateNetworkInterface_WithExplicitIP(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:         aws.String(subnetID),
		PrivateIpAddress: aws.String("10.0.1.100"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.100", *out.NetworkInterface.PrivateIpAddress)
}

func TestAttachENI_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.AttachENI(testAccountID, "eni-nonexistent", "i-test", 0)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestDetachENI_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	err := svc.DetachENI(context.Background(), testAccountID, "eni-nonexistent")
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

// --- Per-account isolation tests ---

func TestEnsureDefaultVPC_PerAccountIsolation(t *testing.T) {
	svc := setupTestVPCService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	_, err := svc.EnsureDefaultVPC(accountA)
	require.NoError(t, err)
	_, err = svc.EnsureDefaultVPC(accountB)
	require.NoError(t, err)

	// Each account should see only their own default VPC
	descA, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, accountA)
	require.NoError(t, err)
	require.Len(t, descA.Vpcs, 1)
	assert.Equal(t, accountA, *descA.Vpcs[0].OwnerId)

	descB, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, accountB)
	require.NoError(t, err)
	require.Len(t, descB.Vpcs, 1)
	assert.Equal(t, accountB, *descB.Vpcs[0].OwnerId)

	// VPC IDs should be different
	assert.NotEqual(t, *descA.Vpcs[0].VpcId, *descB.Vpcs[0].VpcId)
}

func TestEnsureDefaultVPC_IndependentVNIs(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	accountA := "111111111111"
	accountB := "222222222222"

	// Capture VNIs from vpc.create events
	vniCh := make(chan int64, 2)
	sub, err := nc.Subscribe("vpc.create", func(msg *nats.Msg) {
		var evt struct {
			VNI int64 `json:"vni"`
		}
		if err := json.Unmarshal(msg.Data, &evt); err == nil {
			vniCh <- evt.VNI
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.EnsureDefaultVPC(accountA)
	require.NoError(t, err)
	_, err = svc.EnsureDefaultVPC(accountB)
	require.NoError(t, err)

	var vnis []int64
	for range 2 {
		select {
		case vni := <-vniCh:
			vnis = append(vnis, vni)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for vpc.create event")
		}
	}
	assert.NotEqual(t, vnis[0], vnis[1], "each account should get a unique VNI")
}

func TestDescribeVpcs_NoGlobalSharing(t *testing.T) {
	svc := setupTestVPCService(t)
	globalAccount := "000000000000"
	otherAccount := "111111111111"

	// Create default VPC for global account only
	_, err := svc.EnsureDefaultVPC(globalAccount)
	require.NoError(t, err)

	// Other account should NOT see the global default VPC
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, otherAccount)
	require.NoError(t, err)
	assert.Empty(t, desc.Vpcs)
}

func TestGetDefaultSubnet_PerAccount(t *testing.T) {
	svc := setupTestVPCService(t)
	accountA := "111111111111"
	accountB := "222222222222"

	_, err := svc.EnsureDefaultVPC(accountA)
	require.NoError(t, err)
	_, err = svc.EnsureDefaultVPC(accountB)
	require.NoError(t, err)

	subA, err := svc.GetDefaultSubnet(accountA)
	require.NoError(t, err)
	subB, err := svc.GetDefaultSubnet(accountB)
	require.NoError(t, err)

	assert.NotEqual(t, subA.SubnetId, subB.SubnetId)
	assert.Equal(t, "172.31.0.0/20", subA.CidrBlock)
	assert.Equal(t, "172.31.0.0/20", subB.CidrBlock)
}

// --- EnsureDefaultVPC event test ---

func TestEnsureDefaultVPC_PublishesEvents(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)

	vpcCh := make(chan *nats.Msg, 1)
	subCh := make(chan *nats.Msg, 1)
	vpcSub, err := nc.Subscribe("vpc.create", func(msg *nats.Msg) { vpcCh <- msg })
	require.NoError(t, err)
	defer func() { _ = vpcSub.Unsubscribe() }()
	subSub, err := nc.Subscribe("vpc.create-subnet", func(msg *nats.Msg) { subCh <- msg })
	require.NoError(t, err)
	defer func() { _ = subSub.Unsubscribe() }()

	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	// Should publish vpc.create event
	select {
	case msg := <-vpcCh:
		assert.Contains(t, string(msg.Data), "172.31.0.0/16")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create event from EnsureDefaultVPC")
	}

	// Should publish vpc.create-subnet event
	select {
	case msg := <-subCh:
		assert.Contains(t, string(msg.Data), "172.31.0.0/20")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create-subnet event from EnsureDefaultVPC")
	}
}

// --- MapPublicIpOnLaunch tests ---

func TestSubnet_MapPublicIpOnLaunch(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Verify MapPublicIpOnLaunch defaults to false
	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Subnets, 1)
	assert.False(t, *desc.Subnets[0].MapPublicIpOnLaunch)
}

func TestSubnet_ModifyAttribute(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Set MapPublicIpOnLaunch to true
	_, err := svc.ModifySubnetAttribute(context.Background(), &ec2.ModifySubnetAttributeInput{
		SubnetId: aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeSubnets
	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Subnets, 1)
	assert.True(t, *desc.Subnets[0].MapPublicIpOnLaunch)
}

// --- VPC Attribute Tests ---

func TestVpc_DescribeVpcAttribute_Defaults(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// EnableDnsSupport defaults to true
	desc, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, vpcID, *desc.VpcId)
	assert.True(t, *desc.EnableDnsSupport.Value)

	// EnableDnsHostnames defaults to false
	desc, err = svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsHostnames),
	}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *desc.EnableDnsHostnames.Value)

	// EnableNetworkAddressUsageMetrics defaults to false
	desc, err = svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableNetworkAddressUsageMetrics),
	}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *desc.EnableNetworkAddressUsageMetrics.Value)
}

func TestVpc_ModifyVpcAttribute_EnableDnsHostnames(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Set EnableDnsHostnames to true
	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeVpcAttribute
	desc, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsHostnames),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *desc.EnableDnsHostnames.Value)
}

func TestVpc_ModifyVpcAttribute_EnableDnsSupport(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Set EnableDnsSupport to false
	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeVpcAttribute
	desc, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *desc.EnableDnsSupport.Value)
}

func TestVpc_ModifyVpcAttribute_IndependentFields(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Modify only EnableDnsHostnames — EnableDnsSupport should remain true
	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *desc.EnableDnsSupport.Value, "EnableDnsSupport should remain true")
}

func TestVpc_DescribeVpcAttribute_InvalidVpcID(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String("vpc-nonexistent"),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_DescribeVpcAttribute_InvalidAttribute(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String("invalidAttribute"),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_DescribeVpcAttribute_MissingAttribute(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_ModifyVpcAttribute_EnableNetworkAddressUsageMetrics(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId:                            aws.String(vpcID),
		EnableNetworkAddressUsageMetrics: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeVpcAttribute(context.Background(), &ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableNetworkAddressUsageMetrics),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *desc.EnableNetworkAddressUsageMetrics.Value)
}

func TestVpc_ModifyVpcAttribute_NoAttributes(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.EqualError(t, err, "InvalidParameterValue")
}

func TestVpc_ModifyVpcAttribute_InvalidVpcID(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.ModifyVpcAttribute(context.Background(), &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String("vpc-nonexistent"),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	assert.Error(t, err)
}

// --- DescribeVpcs filter tests ---

func TestDescribeVpcs_FilterByCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "172.16.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.0.0/16")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)
	assert.Equal(t, "10.0.0.0/16", *out.Vpcs[0].CidrBlock)
}

func TestDescribeVpcs_FilterByState(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	// VPCs are always "available"
	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("pending")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpcs)
}

func TestDescribeVpcs_FilterByIsDefault(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("is-default"), Values: []*string{aws.String("false")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("is-default"), Values: []*string{aws.String("true")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpcs)
}

func TestDescribeVpcs_FilterMultipleValues_OR(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "172.16.0.0/16")
	createTestVPC(t, svc, "192.168.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.0.0/16"), aws.String("192.168.0.0/16")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 2)
}

func TestDescribeVpcs_FilterMultipleFilters_AND(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	// Both match
	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.0.0/16")}},
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	// One doesn't match
	out, err = svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.0.0/16")}},
			{Name: aws.String("state"), Values: []*string{aws.String("pending")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpcs)
}

func TestDescribeVpcs_FilterUnknownName_Error(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("val")}},
		},
	}, testAccountID)
	require.Error(t, err)
}

func TestDescribeVpcs_FilterWildcard(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "10.1.0.0/16")
	createTestVPC(t, svc, "172.16.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 2)
}

func TestDescribeVpcs_FilterNoResults(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("99.99.99.99/32")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpcs)
}

func TestDescribeVpcs_FilterNoFilters(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "172.16.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 2)
}

func TestDescribeVpcs_FilterByTag(t *testing.T) {
	svc := setupTestVPCService(t)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Environment"), Value: aws.String("prod")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	_ = *out.Vpc.VpcId

	createTestVPC(t, svc, "172.16.0.0/16")

	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Environment"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 1)
	assert.Equal(t, "10.0.0.0/16", *desc.Vpcs[0].CidrBlock)
}

func TestDescribeVpcs_FilterByVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	id1 := createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "172.16.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(id1)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)
	assert.Equal(t, id1, *out.Vpcs[0].VpcId)
}

func TestDescribeVpcs_FilterByOwnerId(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String(testAccountID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String("999999999999")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Vpcs)
}

// --- DescribeSubnets filter tests ---

func TestDescribeSubnets_FilterByVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "172.16.0.0/16")
	createTestSubnet(t, svc, vpc1, "10.0.1.0/24")
	createTestSubnet(t, svc, vpc2, "172.16.1.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)
	assert.Equal(t, vpc1, *out.Subnets[0].VpcId)
}

func TestDescribeSubnets_FilterByCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.1.0/24")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)
	assert.Equal(t, "10.0.1.0/24", *out.Subnets[0].CidrBlock)
}

func TestDescribeSubnets_FilterByState(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)

	out, err = svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("pending")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Subnets)
}

func TestDescribeSubnets_FilterBySubnetId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("subnet-id"), Values: []*string{aws.String(subnetID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)
	assert.Equal(t, subnetID, *out.Subnets[0].SubnetId)
}

func TestDescribeSubnets_FilterByDefaultForAz(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("default-for-az"), Values: []*string{aws.String("false")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)
}

func TestDescribeSubnets_FilterMultipleValues_OR(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.3.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.1.0/24"), aws.String("10.0.3.0/24")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 2)
}

func TestDescribeSubnets_FilterMultipleFilters_AND(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "172.16.0.0/16")
	createTestSubnet(t, svc, vpc1, "10.0.1.0/24")
	createTestSubnet(t, svc, vpc2, "172.16.1.0/24")

	// Both filters match subnet in vpc1
	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.1.0/24")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)

	// Mismatched: vpc1 + wrong cidr
	out, err = svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("172.16.1.0/24")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Subnets)
}

func TestDescribeSubnets_FilterUnknownName_Error(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeSubnets_FilterWildcard(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 2)
}

func TestDescribeSubnets_FilterNoResults(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("192.168.0.0/16")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Subnets)
}

func TestDescribeSubnets_FilterByTag(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Create subnet with tags
	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/24"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("subnet"),
				Tags:         []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Create another without tag
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	desc, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Subnets, 1)
	assert.Equal(t, *out.Subnet.SubnetId, *desc.Subnets[0].SubnetId)
}

// --- SetExternalIPAM / GetSubnet ---

func TestGetSubnet_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	rec, err := svc.GetSubnet(testAccountID, subnetID)
	require.NoError(t, err)
	assert.Equal(t, subnetID, rec.SubnetId)
	assert.Equal(t, vpcID, rec.VpcId)
	assert.Equal(t, "10.0.1.0/24", rec.CidrBlock)
}

func TestGetSubnet_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.GetSubnet(testAccountID, "subnet-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subnet-missing")
}

// --- CreateTags write-through ---

func TestApplyRecordTags_SubnetTagFilteredDescribe(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	err := svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(subnetID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("kubernetes.io/role/elb"), Value: aws.String("1")},
		},
	}, testAccountID)
	require.NoError(t, err)

	// DescribeSubnets must surface the tag...
	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Subnets, 1)
	assert.Equal(t, "1", findTag(out.Subnets[0].Tags, "kubernetes.io/role/elb"))

	// ...and a tag: filter must match it (the LBC auto-discovery path).
	filtered, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:kubernetes.io/role/elb"), Values: []*string{aws.String("1")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, filtered.Subnets, 1)
	assert.Equal(t, subnetID, *filtered.Subnets[0].SubnetId)
}

func TestApplyRecordTags_VpcMergePreservesRecord(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.1.0.0/16")

	err := svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpcs, 1)
	// Merge must not clobber the rest of the record.
	assert.Equal(t, "10.1.0.0/16", *out.Vpcs[0].CidrBlock)
	assert.Equal(t, "prod", findTag(out.Vpcs[0].Tags, "Name"))
}

func TestRemoveRecordTags_Subnet(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.2.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.2.1.0/24")

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(subnetID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(subnetID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Subnets, 1)
	assert.Equal(t, "yes", findTag(out.Subnets[0].Tags, "keep"))
	assert.Empty(t, findTag(out.Subnets[0].Tags, "drop"))
}

func TestApplyRecordTags_SecurityGroupTagFilteredDescribe(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.3.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mirror-sg")

	err := svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(sgID)},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, sgID, *out.SecurityGroups[0].GroupId)
	assert.Equal(t, "prod", findTag(out.SecurityGroups[0].Tags, "env"))
}

func TestRemoveRecordTags_SecurityGroup(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.4.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mirror-sg-del")

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(sgID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(sgID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, "yes", findTag(out.SecurityGroups[0].Tags, "keep"))
	assert.Empty(t, findTag(out.SecurityGroups[0].Tags, "drop"))
}

func TestApplyRecordTags_ENITagFilteredDescribe(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.5.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.5.1.0/24")
	eniID := createTestENI(t, svc, subnetID)

	err := svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(eniID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("primary")}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Name"), Values: []*string{aws.String("primary")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eniID, *out.NetworkInterfaces[0].NetworkInterfaceId)
	assert.Equal(t, "primary", findTag(out.NetworkInterfaces[0].TagSet, "Name"))
}

func TestApplyRecordTags_UnknownResourceNoError(t *testing.T) {
	svc := setupTestVPCService(t)
	// Absent subnet + non-VPC-owned resource id: both skipped without error.
	err := svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("subnet-doesnotexist"), aws.String("i-instance")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.NoError(t, err)
}

func findTag(tags []*ec2.Tag, key string) string {
	for _, t := range tags {
		if t.Key != nil && *t.Key == key {
			return aws.StringValue(t.Value)
		}
	}
	return ""
}
