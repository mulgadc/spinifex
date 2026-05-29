package handlers_ec2_natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestService(t *testing.T) *NatGatewayServiceImpl {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	// Seed VPC KV
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		utils.AccountKey(testAccountID, "vpc-test1"): []byte(`{"vpc_id":"vpc-test1","cidr_block":"10.0.0.0/16","state":"available"}`),
	})

	// Seed subnet KV (public subnet)
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		utils.AccountKey(testAccountID, "subnet-pub1"): []byte(`{"subnet_id":"subnet-pub1","vpc_id":"vpc-test1","cidr_block":"10.0.1.0/24","state":"available","map_public_ip_on_launch":true}`),
	})

	// Seed EIP KV
	testutil.SeedKV(t, js, handlers_ec2_eip.KVBucketEIPs, map[string][]byte{
		utils.AccountKey(testAccountID, "eipalloc-test1"): []byte(`{"allocation_id":"eipalloc-test1","public_ip":"203.0.113.50","state":"allocated"}`),
		utils.AccountKey(testAccountID, "eipalloc-used"):  []byte(`{"allocation_id":"eipalloc-used","public_ip":"203.0.113.51","state":"associated","association_id":"eipassoc-xxx"}`),
	})

	svc, err := NewNatGatewayServiceImplWithNATS(nc)
	require.NoError(t, err)
	return svc
}

func TestCreateNatGateway(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	ngw := out.NatGateway
	assert.NotEmpty(t, *ngw.NatGatewayId)
	assert.Equal(t, "vpc-test1", *ngw.VpcId)
	assert.Equal(t, "subnet-pub1", *ngw.SubnetId)
	assert.Equal(t, "available", *ngw.State)
	require.Len(t, ngw.NatGatewayAddresses, 1)
	assert.Equal(t, "203.0.113.50", *ngw.NatGatewayAddresses[0].PublicIp)
}

func TestCreateNatGateway_SubnetNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-nope"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidSubnetIDNotFound)
}

func TestCreateNatGateway_EIPNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-nope"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidAllocationIDNotFound)
}

func TestCreateNatGateway_EIPAlreadyAssociated(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-used"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorResourceAlreadyAssociated)
}

func TestDeleteNatGateway(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	natgwID := *createOut.NatGateway.NatGatewayId

	deleteOut, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(natgwID),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, natgwID, *deleteOut.NatGatewayId)

	// Should be gone
	_, err = svc.GetNatGateway(testAccountID, natgwID)
	assert.EqualError(t, err, awserrors.ErrorInvalidNatGatewayIDNotFound)
}

func TestDeleteNatGateway_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String("nat-nope"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidNatGatewayIDNotFound)
}

func TestDescribeNatGateways(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)
	assert.Equal(t, *createOut.NatGateway.NatGatewayId, *out.NatGateways[0].NatGatewayId)
}

func TestDescribeNatGateways_ByID(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	natgwID := *createOut.NatGateway.NatGatewayId

	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String(natgwID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)

	// Non-existent ID
	_, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String("nat-nope")},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidNatGatewayIDNotFound)
}

func TestDescribeNatGateways_FilterByState(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	name := "state"
	val := "available"
	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: &name, Values: []*string{&val}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)

	val2 := "deleted"
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: &name, Values: []*string{&val2}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NatGateways)
}

func TestGetNatGateway(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	natgwID := *createOut.NatGateway.NatGatewayId

	record, err := svc.GetNatGateway(testAccountID, natgwID)
	require.NoError(t, err)
	assert.Equal(t, natgwID, record.NatGatewayId)
	assert.Equal(t, "203.0.113.50", record.PublicIp)
	assert.Equal(t, "eipalloc-test1", record.AllocationId)
}

func TestGetNatGateway_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.GetNatGateway(testAccountID, "nat-nope")
	assert.EqualError(t, err, awserrors.ErrorInvalidNatGatewayIDNotFound)
}

func TestCreateNatGateway_MissingParams(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId: aws.String("subnet-pub1"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeNatGateways_DeletedGatewayVisible(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	natgwID := *createOut.NatGateway.NatGatewayId

	_, err = svc.DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(natgwID),
	}, testAccountID)
	require.NoError(t, err)

	// Describe by ID should return the deleted gateway with state=deleted
	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String(natgwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NatGateways, 1)
	assert.Equal(t, "deleted", *out.NatGateways[0].State)

	// Describe without filter should NOT include deleted gateways
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NatGateways)

	// Non-existent ID should still error
	_, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String("nat-never-existed")},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidNatGatewayIDNotFound)
}

func TestDescribeNatGateways_FilterByNatGatewayId(t *testing.T) {
	svc := setupTestService(t)
	createOut, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	natgwID := *createOut.NatGateway.NatGatewayId

	// Exact match
	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: aws.String("nat-gateway-id"), Values: []*string{aws.String(natgwID)}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NatGateways, 1)
	assert.Equal(t, natgwID, *out.NatGateways[0].NatGatewayId)

	// Non-match
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: aws.String("nat-gateway-id"), Values: []*string{aws.String("nat-000000")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NatGateways)

	// Wildcard
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: aws.String("nat-gateway-id"), Values: []*string{aws.String("nat-*")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)
}

func TestDescribeNatGateways_FilterBySubnetId(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Exact match
	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: aws.String("subnet-id"), Values: []*string{aws.String("subnet-pub1")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)

	// Non-match
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: aws.String("subnet-id"), Values: []*string{aws.String("subnet-000000")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NatGateways)
}

func TestDescribeNatGateways_FilterByVpcId(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-pub1"),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	name := "vpc-id"
	val := "vpc-test1"
	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: &name, Values: []*string{&val}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NatGateways, 1)

	val2 := "vpc-nope"
	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{{Name: &name, Values: []*string{&val2}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NatGateways)
}
