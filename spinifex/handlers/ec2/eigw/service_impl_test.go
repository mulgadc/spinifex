package handlers_ec2_eigw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestEIGWService(t *testing.T) *EgressOnlyIGWServiceImpl {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	// Create VPC KV bucket and register test VPCs so fail-closed ownership checks pass
	vpcEntries := map[string][]byte{}
	for _, vpcID := range []string{"vpc-test123", "vpc-tagged", "vpc-tagged2"} {
		vpcEntries[utils.AccountKey(testAccountID, vpcID)] = []byte(`{"vpc_id":"` + vpcID + `","state":"available"}`)
	}
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, vpcEntries)

	svc, err := NewEgressOnlyIGWServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc
}

func createTestEIGW(t *testing.T, svc *EgressOnlyIGWServiceImpl) string {
	t.Helper()
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)
	return *out.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId
}

func TestCreateEgressOnlyInternetGateway(t *testing.T) {
	svc := setupTestEIGWService(t)
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String("vpc-test123"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.EgressOnlyInternetGateway)
	assert.Equal(t, "eigw-", (*out.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId)[:5])
	require.NotEmpty(t, out.EgressOnlyInternetGateway.Attachments)
	assert.Equal(t, "vpc-test123", *out.EgressOnlyInternetGateway.Attachments[0].VpcId)
	assert.Equal(t, "attached", *out.EgressOnlyInternetGateway.Attachments[0].State)
}

func TestCreateEgressOnlyInternetGateway_MissingVpcId(t *testing.T) {
	svc := setupTestEIGWService(t)
	_, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateEgressOnlyInternetGateway_EmptyVpcId(t *testing.T) {
	svc := setupTestEIGWService(t)
	_, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String(""),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateEgressOnlyInternetGateway_WithTags(t *testing.T) {
	svc := setupTestEIGWService(t)
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String("vpc-tagged"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("egress-only-internet-gateway"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-eigw")},
					{Key: aws.String("Env"), Value: aws.String("test")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.EgressOnlyInternetGateway)
	assert.Len(t, out.EgressOnlyInternetGateway.Tags, 2)

	// Verify tags persist through describe
	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		EgressOnlyInternetGatewayIds: []*string{out.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.EgressOnlyInternetGateways, 1)
	assert.Len(t, desc.EgressOnlyInternetGateways[0].Tags, 2)
}

func TestCreateEgressOnlyInternetGateway_TagsWrongResourceType(t *testing.T) {
	svc := setupTestEIGWService(t)
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String("vpc-tagged2"),
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
	assert.Empty(t, out.EgressOnlyInternetGateway.Tags)
}

func TestDeleteEgressOnlyInternetGateway(t *testing.T) {
	svc := setupTestEIGWService(t)
	eigwID := createTestEIGW(t, svc)

	out, err := svc.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
		EgressOnlyInternetGatewayId: aws.String(eigwID),
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *out.ReturnCode)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		EgressOnlyInternetGatewayIds: []*string{aws.String(eigwID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.EgressOnlyInternetGateways)
}

func TestDeleteEgressOnlyInternetGateway_NotFound(t *testing.T) {
	svc := setupTestEIGWService(t)
	_, err := svc.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
		EgressOnlyInternetGatewayId: aws.String("eigw-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidEgressOnlyInternetGatewayId.NotFound")
}

func TestDeleteEgressOnlyInternetGateway_MissingID(t *testing.T) {
	svc := setupTestEIGWService(t)
	_, err := svc.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDeleteEgressOnlyInternetGateway_EmptyID(t *testing.T) {
	svc := setupTestEIGWService(t)
	_, err := svc.DeleteEgressOnlyInternetGateway(&ec2.DeleteEgressOnlyInternetGatewayInput{
		EgressOnlyInternetGatewayId: aws.String(""),
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestDescribeEgressOnlyInternetGateways_All(t *testing.T) {
	svc := setupTestEIGWService(t)
	createTestEIGW(t, svc)
	createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.EgressOnlyInternetGateways, 2)
}

func TestDescribeEgressOnlyInternetGateways_ByID(t *testing.T) {
	svc := setupTestEIGWService(t)
	eigwID := createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		EgressOnlyInternetGatewayIds: []*string{aws.String(eigwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.EgressOnlyInternetGateways, 1)
	assert.Equal(t, eigwID, *desc.EgressOnlyInternetGateways[0].EgressOnlyInternetGatewayId)
}

func TestDescribeEgressOnlyInternetGateways_Empty(t *testing.T) {
	svc := setupTestEIGWService(t)
	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.EgressOnlyInternetGateways)
}

func TestDescribeEgressOnlyInternetGateways_FilterByEIGWId(t *testing.T) {
	svc := setupTestEIGWService(t)
	eigwID := createTestEIGW(t, svc)
	createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("egress-only-internet-gateway-id"), Values: []*string{aws.String(eigwID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.EgressOnlyInternetGateways, 1)
	assert.Equal(t, eigwID, *desc.EgressOnlyInternetGateways[0].EgressOnlyInternetGatewayId)
}

func TestDescribeEgressOnlyInternetGateways_FilterMultipleValues_OR(t *testing.T) {
	svc := setupTestEIGWService(t)
	eigwID1 := createTestEIGW(t, svc)
	eigwID2 := createTestEIGW(t, svc)
	createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("egress-only-internet-gateway-id"), Values: []*string{aws.String(eigwID1), aws.String(eigwID2)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.EgressOnlyInternetGateways, 2)
}

func TestDescribeEgressOnlyInternetGateways_FilterUnknownName_Error(t *testing.T) {
	svc := setupTestEIGWService(t)

	_, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeEgressOnlyInternetGateways_FilterWildcard(t *testing.T) {
	svc := setupTestEIGWService(t)
	createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("egress-only-internet-gateway-id"), Values: []*string{aws.String("eigw-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.EgressOnlyInternetGateways, 1)
}

func TestDescribeEgressOnlyInternetGateways_FilterNoResults(t *testing.T) {
	svc := setupTestEIGWService(t)
	createTestEIGW(t, svc)

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("egress-only-internet-gateway-id"), Values: []*string{aws.String("eigw-nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.EgressOnlyInternetGateways)
}

func TestDescribeEgressOnlyInternetGateways_FilterByTag(t *testing.T) {
	svc := setupTestEIGWService(t)
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String("vpc-tagged"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("egress-only-internet-gateway"),
				Tags:         []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	createTestEIGW(t, svc) // untagged

	desc, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.EgressOnlyInternetGateways, 1)
	assert.Equal(t, *out.EgressOnlyInternetGateway.EgressOnlyInternetGatewayId, *desc.EgressOnlyInternetGateways[0].EgressOnlyInternetGatewayId)
}

// TestCreateEgressOnlyInternetGateway_CrossAccountVPCRejected tests that creating an EIGW in another account's VPC is rejected.
func TestCreateEgressOnlyInternetGateway_CrossAccountVPCRejected(t *testing.T) {
	// Set up with manual NATS to get VPC KV access
	_, nc, js := testutil.StartTestJetStream(t)

	// Create VPC KV bucket with a VPC owned by testAccountID
	vpcID := "vpc-alpha456"
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		utils.AccountKey(testAccountID, vpcID): []byte(`{"vpc_id":"vpc-alpha456","state":"available"}`),
	})

	var err error
	require.NoError(t, err)

	// Create EIGW service (VPC KV will be populated since we created it before EIGW init)
	svc, err := NewEgressOnlyIGWServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	// Other account tries to create EIGW in testAccountID's VPC — should fail
	otherAccount := "999999999999"
	_, err = svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String(vpcID),
	}, otherAccount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidVpcID.NotFound")

	// Owner creates EIGW in their own VPC — should succeed
	out, err := svc.CreateEgressOnlyInternetGateway(&ec2.CreateEgressOnlyInternetGatewayInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, out.EgressOnlyInternetGateway)
}
