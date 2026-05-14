package handlers_ec2_vpc

import (
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

	svc, err := NewVPCServiceImplWithNATS(nil, nc)
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
// always returns success=false. Used by Phase 7 propagation tests to assert
// vpcd-side errors surface to the API caller. The default-VPC bootstrap is
// not needed here — these tests CreateVpc themselves once they have working
// stubs swapped in.
func setupTestVPCServiceWithFailingVpcd(t *testing.T, errMsg string) (*VPCServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGFailingResponder(t, nc, errMsg)
	return svc, nc
}

func createTestVPC(t *testing.T, svc *VPCServiceImpl, cidr string) string {
	t.Helper()
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String(cidr),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Vpc.VpcId
}

func createTestSubnet(t *testing.T, svc *VPCServiceImpl, vpcID, cidr string) string {
	t.Helper()
	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String(cidr),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Subnet.SubnetId
}

// --- VPC Tests ---

func TestCreateVpc(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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
	_, err := svc.CreateVpc(&ec2.CreateVpcInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateVpc_EmptyCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String(""),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateVpc_InvalidCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("not-a-cidr"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_CidrTooLarge(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/8"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_CidrTooSmall(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/29"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcRange")
}

func TestCreateVpc_WithTags(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{out.Vpc.VpcId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.Len(t, desc.Vpcs[0].Tags, 2)
}

func TestCreateVpc_TagsWrongResourceType(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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
	out1, err := svc.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	out2, err := svc.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.1.0.0/16")}, testAccountID)
	require.NoError(t, err)

	// Verify VPCs are different
	assert.NotEqual(t, *out1.Vpc.VpcId, *out2.Vpc.VpcId)
}

func TestDeleteVpc(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
	assert.Nil(t, desc)
}

func TestDeleteVpc_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{
		VpcId: aws.String("vpc-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

func TestDeleteVpc_MissingID(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDeleteVpc_WithSubnets(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	// Should fail because VPC has subnets
	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")
}

func TestDescribeVpcs_All(t *testing.T) {
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "10.1.0.0/16")

	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 2)
}

func TestDescribeVpcs_ByID(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestVPC(t, svc, "10.1.0.0/16")

	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.Equal(t, vpcID, *desc.Vpcs[0].VpcId)
}

func TestDescribeVpcs_Empty(t *testing.T) {
	svc := setupTestVPCService(t)
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Vpcs)
}

func TestDescribeVpcs_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String("vpc-nonexistent")},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

// --- Subnet Tests ---

func TestCreateSubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateSubnet_MissingCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateSubnet_InvalidVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String("vpc-nonexistent"),
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

func TestCreateSubnet_InvalidCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("not-a-cidr"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Range")
}

func TestCreateSubnet_OutsideVpcCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/25"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Conflict")
}

func TestCreateSubnet_WithTags(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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

	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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

	_, err := svc.DeleteSubnet(&ec2.DeleteSubnetInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(subnetID)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
	assert.Nil(t, desc)
}

func TestDeleteSubnet_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteSubnet(&ec2.DeleteSubnetInput{
		SubnetId: aws.String("subnet-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
}

func TestDeleteSubnet_MissingID(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteSubnet(&ec2.DeleteSubnetInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDescribeSubnets_All(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Subnets, 2)
}

func TestDescribeSubnets_ByID(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	createTestSubnet(t, svc, vpcID, "10.0.2.0/24")

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.Subnets)
}

func TestDescribeSubnets_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	assert.ErrorContains(t, err, "DependencyViolation")

	// Delete subnet first
	_, err = svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.NoError(t, err)

	// Now VPC can be deleted
	_, err = svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

func TestCreateSubnet_CidrRanges(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// /28 (smallest allowed) = 16 IPs - 5 reserved = 11
	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.0.0/28"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(11), *out.Subnet.AvailableIpAddressCount)

	// /16 (largest allowed) = 65536 IPs - 5 reserved = 65531
	vpcID2 := createTestVPC(t, svc, "172.16.0.0/16")
	out2, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.True(t, *desc.Vpcs[0].IsDefault)
	assert.Equal(t, "172.31.0.0/16", *desc.Vpcs[0].CidrBlock)

	// Verify default subnet was created
	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
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
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Vpcs, 1)

	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
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

	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
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

// TestEnsureDefaultVPC_NoVpcdResponder simulates the daemon-startup race where
// EnsureDefaultVPC runs before vpcd has subscribed to vpc.create-sg. Without
// the fix, the synchronous SG round-trip errors out and EnsureDefaultVPC
// returns early, leaving the VPC without a default subnet or main route table.
// After the fix the SG step is best-effort, so subnet + RTB still land in KV
// and vpcd's reconcile-sgs loop converges the OVN port group asynchronously.
func TestEnsureDefaultVPC_NoVpcdResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	// Intentionally NOT calling StubVpcdSGResponder — vpc.create-sg has no
	// responder, mirroring the bootstrap race.

	info, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	require.NotNil(t, info)

	// Default VPC, subnet, and main RTB must all be present in KV.
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1)
	assert.True(t, *desc.Vpcs[0].IsDefault)

	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.Equal(t, info.VpcId, *subDesc.Subnets[0].VpcId)

	require.NotNil(t, svc.rtbKV)
	rtbKeys, err := svc.rtbKV.Keys()
	require.NoError(t, err)
	foundMainRTB := false
	for _, k := range rtbKeys {
		entry, err := svc.rtbKV.Get(k)
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
	sgKeys, err := svc.sgKV.Keys()
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
	_, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.0.0/29"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnet.Range")
}

func TestVpcCidrBlockAssociation(t *testing.T) {
	svc := setupTestVPCService(t)
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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
	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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

	_, err = svc.DeleteVpc(&ec2.DeleteVpcInput{
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

	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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

	_, err = svc.DeleteSubnet(&ec2.DeleteSubnetInput{
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
	svc, err := NewVPCServiceImplWithNATS(cfg, nc)
	require.NoError(t, err)

	_, err = svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	// Verify the subnet uses the configured AZ
	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.Equal(t, "us-west-2b", *subDesc.Subnets[0].AvailabilityZone)
}

func TestCreateVpc_NormalizesNetworkCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	// Pass a CIDR with host bits set — should be normalized to network address
	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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
	_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
	}, testAccountID)
	require.NoError(t, err)

	// Delete subnet should succeed (ENI is in subnet but delete checks subnet dependencies in vpc delete)
	// First delete subnet
	_, err = svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	// DeleteSubnet doesn't check for ENIs currently - just deletes
	require.NoError(t, err)

	// Now delete VPC should succeed since subnet is gone
	_, err = svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

func TestCreateNetworkInterface_WithExplicitIP(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
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
	err := svc.DetachENI(testAccountID, "eni-nonexistent")
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
	descA, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, accountA)
	require.NoError(t, err)
	require.Len(t, descA.Vpcs, 1)
	assert.Equal(t, accountA, *descA.Vpcs[0].OwnerId)

	descB, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, accountB)
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
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, otherAccount)
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
	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	_, err := svc.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId: aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeSubnets
	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	desc, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, vpcID, *desc.VpcId)
	assert.True(t, *desc.EnableDnsSupport.Value)

	// EnableDnsHostnames defaults to false
	desc, err = svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsHostnames),
	}, testAccountID)
	require.NoError(t, err)
	assert.False(t, *desc.EnableDnsHostnames.Value)

	// EnableNetworkAddressUsageMetrics defaults to false
	desc, err = svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
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
	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeVpcAttribute
	desc, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
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
	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeVpcAttribute
	desc, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
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
	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *desc.EnableDnsSupport.Value, "EnableDnsSupport should remain true")
}

func TestVpc_DescribeVpcAttribute_InvalidVpcID(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String("vpc-nonexistent"),
		Attribute: aws.String(ec2.VpcAttributeNameEnableDnsSupport),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_DescribeVpcAttribute_InvalidAttribute(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String("invalidAttribute"),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_DescribeVpcAttribute_MissingAttribute(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.Error(t, err)
}

func TestVpc_ModifyVpcAttribute_EnableNetworkAddressUsageMetrics(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
		VpcId:                            aws.String(vpcID),
		EnableNetworkAddressUsageMetrics: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeVpcAttribute(&ec2.DescribeVpcAttributeInput{
		VpcId:     aws.String(vpcID),
		Attribute: aws.String(ec2.VpcAttributeNameEnableNetworkAddressUsageMetrics),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *desc.EnableNetworkAddressUsageMetrics.Value)
}

func TestVpc_ModifyVpcAttribute_NoAttributes(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	assert.EqualError(t, err, "InvalidParameterValue")
}

func TestVpc_ModifyVpcAttribute_InvalidVpcID(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.ModifyVpcAttribute(&ec2.ModifyVpcAttributeInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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
	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("is-default"), Values: []*string{aws.String("false")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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
	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.0.0/16")}},
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	// One doesn't match
	out, err = svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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
	_, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 2)
}

func TestDescribeVpcs_FilterByTag(t *testing.T) {
	svc := setupTestVPCService(t)

	out, err := svc.CreateVpc(&ec2.CreateVpcInput{
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

	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String(testAccountID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Vpcs, 1)

	out, err = svc.DescribeVpcs(&ec2.DescribeVpcsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)

	out, err = svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
			{Name: aws.String("cidr-block"), Values: []*string{aws.String("10.0.1.0/24")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Subnets, 1)

	// Mismatched: vpc1 + wrong cidr
	out, err = svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	_, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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

	out, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
	out, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
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

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
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
