package handlers_ec2_routetable

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestService(t *testing.T) *RouteTableServiceImpl {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	// Seed VPC KV
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		utils.AccountKey(testAccountID, "vpc-test1"): []byte(`{"vpc_id":"vpc-test1","cidr_block":"10.0.0.0/16","state":"available"}`),
	})

	// Seed IGW KV (attached to vpc-test1)
	testutil.SeedKV(t, js, handlers_ec2_igw.KVBucketIGW, map[string][]byte{
		utils.AccountKey(testAccountID, "igw-test1"): []byte(`{"internet_gateway_id":"igw-test1","vpc_id":"vpc-test1","state":"available"}`),
	})

	// Seed subnet KV
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		utils.AccountKey(testAccountID, "subnet-test1"): []byte(`{"subnet_id":"subnet-test1","vpc_id":"vpc-test1","cidr_block":"10.0.1.0/24","state":"available"}`),
		utils.AccountKey(testAccountID, "subnet-priv1"): []byte(`{"subnet_id":"subnet-priv1","vpc_id":"vpc-test1","cidr_block":"10.0.11.0/24","state":"available"}`),
	})

	// Seed NAT Gateway KV
	testutil.SeedKV(t, js, handlers_ec2_natgw.KVBucketNatGateways, map[string][]byte{
		utils.AccountKey(testAccountID, "nat-test1"): []byte(`{"nat_gateway_id":"nat-test1","vpc_id":"vpc-test1","subnet_id":"subnet-test1","public_ip":"192.168.1.100","state":"available"}`),
	})

	svc, err := NewRouteTableServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc
}

func createTestRtb(t *testing.T, svc *RouteTableServiceImpl) string {
	t.Helper()
	out, err := svc.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String("vpc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	return *out.RouteTable.RouteTableId
}

func TestCreateRouteTable(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String("vpc-test1"),
	}, testAccountID)
	require.NoError(t, err)

	rtb := out.RouteTable
	assert.NotEmpty(t, *rtb.RouteTableId)
	assert.Equal(t, "vpc-test1", *rtb.VpcId)
	assert.Empty(t, rtb.Associations) // custom tables have no associations initially

	// Should have local route
	require.Len(t, rtb.Routes, 1)
	assert.Equal(t, "10.0.0.0/16", *rtb.Routes[0].DestinationCidrBlock)
	assert.Equal(t, "local", *rtb.Routes[0].GatewayId)
	assert.Equal(t, "active", *rtb.Routes[0].State)
}

// TestCreateRouteTable_PersistsTagsForTagFilterDiscovery locks mulga-siv-303:
// CreateRouteTable must persist TagSpecifications so the tag-driven CP-VPC
// teardown can find the private route table, clear its NAT-GW route, and let
// the NAT gateway (and its billable EIP) be reclaimed.
func TestCreateRouteTable_PersistsTagsForTagFilterDiscovery(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String("vpc-test1"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeRouteTable),
			Tags: []*ec2.Tag{
				{Key: aws.String("spinifex:eks-cluster"), Value: aws.String("alpha")},
				{Key: aws.String("spinifex:eks-role"), Value: aws.String("cp-private-rt")},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:spinifex:eks-cluster"), Values: aws.StringSlice([]string{"alpha"})},
			{Name: aws.String("tag:spinifex:eks-role"), Values: aws.StringSlice([]string{"cp-private-rt"})},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1, "tagged route table must be discoverable by tag filter (mulga-siv-303)")

	tags := map[string]string{}
	for _, tg := range out.RouteTables[0].Tags {
		tags[aws.StringValue(tg.Key)] = aws.StringValue(tg.Value)
	}
	assert.Equal(t, "alpha", tags["spinifex:eks-cluster"])
	assert.Equal(t, "cp-private-rt", tags["spinifex:eks-role"])
}

func TestCreateRouteTable_VpcNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: aws.String("vpc-nonexistent"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidVpcIDNotFound)
}

func TestDeleteRouteTable(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(rtbID),
	}, testAccountID)
	require.NoError(t, err)

	// Should be gone
	_, err = svc.getRouteTable(testAccountID, rtbID)
	assert.EqualError(t, err, awserrors.ErrorInvalidRouteTableIDNotFound)
}

func TestDeleteRouteTable_Main(t *testing.T) {
	svc := setupTestService(t)
	record, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	_, err = svc.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(record.RouteTableId),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorDependencyViolation)
}

func TestDeleteRouteTable_WithAssociations(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	// Associate a subnet
	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Should fail to delete
	_, err = svc.DeleteRouteTable(&ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(rtbID),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorDependencyViolation)
}

func TestDescribeRouteTables(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)
	assert.Equal(t, rtbID, *out.RouteTables[0].RouteTableId)
}

func TestDescribeRouteTables_FilterByVpcId(t *testing.T) {
	svc := setupTestService(t)
	createTestRtb(t, svc)

	name := "vpc-id"
	val := "vpc-test1"
	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: &name, Values: []*string{&val}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)

	// Filter by non-existent VPC
	val2 := "vpc-nope"
	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: &name, Values: []*string{&val2}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)
}

func TestDescribeRouteTables_FilterByMain(t *testing.T) {
	svc := setupTestService(t)

	// Create a main route table
	_, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	// Create a non-main route table
	createTestRtb(t, svc)

	name := "association.main"
	val := "true"
	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: &name, Values: []*string{&val}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)
	assert.True(t, *out.RouteTables[0].Associations[0].Main)
}

func TestDescribeRouteTables_FilterByRouteState(t *testing.T) {
	svc := setupTestService(t)
	createTestRtb(t, svc) // has local route with state=active

	// Exact match
	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.state"), Values: []*string{aws.String("active")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)

	// Non-match
	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.state"), Values: []*string{aws.String("blackhole")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)
}

func TestDescribeRouteTables_FilterByRouteOrigin(t *testing.T) {
	svc := setupTestService(t)
	createTestRtb(t, svc) // local route has origin=CreateRouteTable

	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.origin"), Values: []*string{aws.String("CreateRouteTable")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)

	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.origin"), Values: []*string{aws.String("CreateRoute")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)
}

func TestDescribeRouteTables_FilterByRouteNatGatewayId(t *testing.T) {
	svc := setupTestService(t)
	createTestRtb(t, svc) // no NAT GW route

	// No match — no routes have a nat-gateway-id
	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.nat-gateway-id"), Values: []*string{aws.String("nat-000000")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)

	// Wildcard on empty field should not match
	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.nat-gateway-id"), Values: []*string{aws.String("nat-*")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)
}

func TestDescribeRouteTables_FilterByOwnerId(t *testing.T) {
	svc := setupTestService(t)
	createTestRtb(t, svc)

	// Exact match
	out, err := svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("owner-id"), Values: []*string{aws.String(testAccountID)}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)

	// Non-match
	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("owner-id"), Values: []*string{aws.String("999999999999")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.RouteTables)

	// Wildcard
	out, err = svc.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("owner-id"), Values: []*string{aws.String("1234*")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.RouteTables, 1)
}

func TestCreateRoute(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify route was added
	record, err := svc.getRouteTable(testAccountID, rtbID)
	require.NoError(t, err)
	assert.Len(t, record.Routes, 2) // local + igw
	assert.Equal(t, "0.0.0.0/0", record.Routes[1].DestinationCidrBlock)
	assert.Equal(t, "igw-test1", record.Routes[1].GatewayId)
}

func TestCreateRoute_DuplicateDestination(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Duplicate should fail
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorRouteAlreadyExists)
}

func TestDeleteRoute(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, testAccountID)
	require.NoError(t, err)

	record, err := svc.getRouteTable(testAccountID, rtbID)
	require.NoError(t, err)
	assert.Len(t, record.Routes, 1) // only local remains
}

func TestDeleteRoute_LocalRoute(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteRoute_NotFound(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("192.168.0.0/16"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidRouteNotFound)
}

func TestReplaceRoute(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Replace target (same IGW for simplicity — validates the swap logic)
	_, err = svc.ReplaceRoute(&ec2.ReplaceRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
}

func TestAssociateRouteTable(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	out, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotEmpty(t, *out.AssociationId)
	assert.Equal(t, "associated", *out.AssociationState.State)
}

func TestAssociateRouteTable_DuplicateSubnet(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// Second association should fail
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorResourceAlreadyAssociated)
}

func TestDisassociateRouteTable(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: assocOut.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	// Verify association removed
	record, err := svc.getRouteTable(testAccountID, rtbID)
	require.NoError(t, err)
	assert.Empty(t, record.Associations)
}

func TestDisassociateRouteTable_Main(t *testing.T) {
	svc := setupTestService(t)
	record, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: aws.String(record.Associations[0].AssociationId),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestReplaceRouteTableAssociation(t *testing.T) {
	svc := setupTestService(t)
	rtb1ID := createTestRtb(t, svc)
	rtb2ID := createTestRtb(t, svc)

	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtb1ID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)

	replaceOut, err := svc.ReplaceRouteTableAssociation(&ec2.ReplaceRouteTableAssociationInput{
		AssociationId: assocOut.AssociationId,
		RouteTableId:  aws.String(rtb2ID),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotEmpty(t, *replaceOut.NewAssociationId)
	assert.NotEqual(t, *assocOut.AssociationId, *replaceOut.NewAssociationId)

	// Verify old table has no associations
	oldRecord, err := svc.getRouteTable(testAccountID, rtb1ID)
	require.NoError(t, err)
	assert.Empty(t, oldRecord.Associations)

	// Verify new table has the association
	newRecord, err := svc.getRouteTable(testAccountID, rtb2ID)
	require.NoError(t, err)
	require.Len(t, newRecord.Associations, 1)
	assert.Equal(t, "subnet-test1", newRecord.Associations[0].SubnetId)
}

func TestCreateRouteTableForVPC_Main(t *testing.T) {
	svc := setupTestService(t)
	record, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	assert.True(t, record.IsMain)
	assert.Len(t, record.Associations, 1)
	assert.True(t, record.Associations[0].Main)
	assert.Len(t, record.Routes, 1)
	assert.Equal(t, "local", record.Routes[0].GatewayId)
}

// receiveNatGWEvent reads one message from sub, decodes, and asserts topic+cidr.
// Returns the decoded public_ip for further assertions.
func receiveNatGWEvent(t *testing.T, sub *nats.Subscription, wantCidr string) (natgwID, publicIp string) {
	t.Helper()
	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err, "expected NAT GW event on %s", sub.Subject)
	var evt struct {
		VpcId           string `json:"vpc_id"`
		NatGatewayId    string `json:"nat_gateway_id"`
		PublicIp        string `json:"public_ip"`
		SubnetCidr      string `json:"subnet_cidr"`
		SubnetId        string `json:"subnet_id"`
		DestinationCidr string `json:"destination_cidr"`
	}
	require.NoError(t, json.Unmarshal(msg.Data, &evt))
	assert.Equal(t, wantCidr, evt.SubnetCidr)
	assert.NotEmpty(t, evt.SubnetId, "subnet_id required so subscriber can install per-subnet egress policy")
	assert.NotEmpty(t, evt.DestinationCidr, "destination_cidr required so subscriber can install per-subnet egress policy")
	return evt.NatGatewayId, evt.PublicIp
}

// TestAssociateRouteTable_PublishesNatGatewayEvent covers the terraform flow where
// AssociateRouteTable must emit a NAT GW event so vpcd installs the SNAT rule.
func TestAssociateRouteTable_PublishesNatGatewayEvent(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	// Add NAT GW route BEFORE any subnet is associated (terraform ordering).
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String("nat-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// No subnet yet → no event from CreateRoute.
	_, err = sub.NextMsg(100 * time.Millisecond)
	assert.Error(t, err, "CreateRoute must not publish when table has no associations")

	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	natgwID, publicIp := receiveNatGWEvent(t, sub, "10.0.11.0/24")
	assert.Equal(t, "nat-test1", natgwID)
	assert.Equal(t, "192.168.1.100", publicIp)
}

// TestAssociateRouteTable_NoNatGatewayRoute ensures Associate stays silent when
// the route table has no NAT GW routes.
func TestAssociateRouteTable_NoNatGatewayRoute(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = sub.NextMsg(100 * time.Millisecond)
	assert.Error(t, err, "Associate must not publish when table has no NAT GW routes")
}

// TestDisassociateRouteTable_PublishesNatGatewayDeleteEvent verifies SNAT
// teardown when a subnet leaves a table with a NAT GW route.
func TestDisassociateRouteTable_PublishesNatGatewayDeleteEvent(t *testing.T) {
	svc := setupTestService(t)
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String("nat-test1"),
	}, testAccountID)
	require.NoError(t, err)

	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: assocOut.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	natgwID, _ := receiveNatGWEvent(t, delSub, "10.0.11.0/24")
	assert.Equal(t, "nat-test1", natgwID)
}

// TestDeleteRoute_NATGW_PublishesDeleteForAssociatedSubnets verifies that every
// associated subnet receives a vpc.delete-nat-gateway event when a NATGW route is removed.
func TestDeleteRoute_NATGW_PublishesDeleteForAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	addSub, err := svc.natsConn.SubscribeSync("vpc.add-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = addSub.Unsubscribe() }()
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String("nat-test1"),
	}, testAccountID)
	require.NoError(t, err)
	// Drain the add event from CreateRoute.
	natgwID, _ := receiveNatGWEvent(t, addSub, "10.0.11.0/24")
	require.Equal(t, "nat-test1", natgwID)

	_, err = svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, testAccountID)
	require.NoError(t, err)

	delNatgwID, _ := receiveNatGWEvent(t, delSub, "10.0.11.0/24")
	assert.Equal(t, "nat-test1", delNatgwID)
}

// TestReplaceRouteTableAssociation_PublishesNatGatewayEvents covers the move
// path: subnet leaves a NAT-routed table for a NAT-free table. We expect a
// delete event against the old table's NAT GW route and no add event.
func TestReplaceRouteTableAssociation_PublishesNatGatewayEvents(t *testing.T) {
	svc := setupTestService(t)
	addSub, err := svc.natsConn.SubscribeSync("vpc.add-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = addSub.Unsubscribe() }()
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-nat-gateway")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	oldRtb := createTestRtb(t, svc)
	newRtb := createTestRtb(t, svc)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(oldRtb),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String("nat-test1"),
	}, testAccountID)
	require.NoError(t, err)

	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(oldRtb),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	// Drain the add event from the initial association.
	_, err = addSub.NextMsg(time.Second)
	require.NoError(t, err)

	_, err = svc.ReplaceRouteTableAssociation(&ec2.ReplaceRouteTableAssociationInput{
		AssociationId: assocOut.AssociationId,
		RouteTableId:  aws.String(newRtb),
	}, testAccountID)
	require.NoError(t, err)

	natgwID, _ := receiveNatGWEvent(t, delSub, "10.0.11.0/24")
	assert.Equal(t, "nat-test1", natgwID)

	// New table has no NAT GW routes → no add event.
	_, err = addSub.NextMsg(100 * time.Millisecond)
	assert.Error(t, err, "Replace must not publish add when new table has no NAT GW routes")
}

func receiveIGWRouteEvent(t *testing.T, sub *nats.Subscription, wantCidr string) (subnetID, igwID, destCidr string) {
	t.Helper()
	msg, err := sub.NextMsg(time.Second)
	require.NoError(t, err)
	var evt struct {
		VpcId             string `json:"vpc_id"`
		SubnetId          string `json:"subnet_id"`
		DestinationCidr   string `json:"destination_cidr"`
		InternetGatewayId string `json:"internet_gateway_id"`
	}
	require.NoError(t, json.Unmarshal(msg.Data, &evt))
	assert.Equal(t, wantCidr, evt.DestinationCidr)
	return evt.SubnetId, evt.InternetGatewayId, evt.DestinationCidr
}

// TestCreateRoute_IGW_PublishesAddIGWRouteForAssociatedSubnets covers the
// CreateRoute(IGW) flow: emits one vpc.add-igw-route event per associated
// subnet so the network subscriber installs per-subnet egress policies.
func TestCreateRoute_IGW_PublishesAddIGWRouteForAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	// Associate subnet FIRST, then CreateRoute — exercises the "route after
	// association" leg.
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, igwID, _ := receiveIGWRouteEvent(t, sub, "0.0.0.0/0")
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "igw-test1", igwID)
}

// TestAssociateRouteTable_PublishesAddIGWRouteForExistingRoute covers the terraform
// ordering: AssociateRouteTable must emit an IGW route event for the joining subnet.
func TestAssociateRouteTable_PublishesAddIGWRouteForExistingRoute(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	// No subnet yet → no event from CreateRoute.
	_, err = sub.NextMsg(100 * time.Millisecond)
	assert.Error(t, err, "CreateRoute(IGW) must not publish when table has no associations")

	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, igwID, _ := receiveIGWRouteEvent(t, sub, "0.0.0.0/0")
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "igw-test1", igwID)
}

// TestDeleteRoute_IGW_PublishesDeleteIGWRoute verifies policy teardown when
// the IGW route is removed from a table with associations.
func TestDeleteRoute_IGW_PublishesDeleteIGWRoute(t *testing.T) {
	svc := setupTestService(t)
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-igw-route")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, igwID, _ := receiveIGWRouteEvent(t, delSub, "0.0.0.0/0")
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "igw-test1", igwID)
}

// TestDisassociateRouteTable_PublishesDeleteIGWRoute verifies the egress policy
// is torn down when a subnet leaves a table that carries an IGW route.
func TestDisassociateRouteTable_PublishesDeleteIGWRoute(t *testing.T) {
	svc := setupTestService(t)
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-igw-route")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: assocOut.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	subnetID, igwID, _ := receiveIGWRouteEvent(t, delSub, "0.0.0.0/0")
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "igw-test1", igwID)
}

// drainIGWSubnets reads up to `count` events from sub and returns the unique
// SubnetIds. Used to assert fan-out shape regardless of NATS message order.
func drainIGWSubnets(t *testing.T, sub *nats.Subscription, count int) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for range count {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var evt struct {
			SubnetId string `json:"subnet_id"`
		}
		require.NoError(t, json.Unmarshal(msg.Data, &evt))
		got[evt.SubnetId] = true
	}
	return got
}

// TestCreateRoute_IGW_OnMainRT_FansOutToImplicitSubnets verifies CreateRoute(IGW) on
// the main RT emits one vpc.add-igw-route per implicit-main subnet.
func TestCreateRoute_IGW_OnMainRT_FansOutToImplicitSubnets(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	mainRT, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT.RouteTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	got := drainIGWSubnets(t, sub, 4)
	assert.True(t, got["subnet-test1"], "implicit main-RT subnet must receive add event")
	assert.True(t, got["subnet-priv1"], "implicit main-RT subnet must receive add event")
	assert.Len(t, got, 2)
}

// TestCreateRoute_IGW_OnMainRT_SkipsExplicitlyAssociatedSubnets ensures a
// subnet with an explicit non-main RT association does NOT receive the main
// RT's IGW route event — its effective route table is the explicit one.
func TestCreateRoute_IGW_OnMainRT_SkipsExplicitlyAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	sub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	mainRT, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	customRtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(customRtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT.RouteTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	got := drainIGWSubnets(t, sub, 4)
	assert.True(t, got["subnet-test1"], "implicit subnet must receive add event")
	assert.False(t, got["subnet-priv1"], "explicitly-associated subnet must NOT receive main-RT event")
}

// TestAssociateRouteTable_RemovesMainRTRoutesForJoiningSubnet verifies that the main
// RT's per-subnet rules are torn down when a subnet joins an explicit non-main RT.
func TestAssociateRouteTable_RemovesMainRTRoutesForJoiningSubnet(t *testing.T) {
	svc := setupTestService(t)
	delSub, err := svc.natsConn.SubscribeSync("vpc.delete-igw-route")
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	mainRT, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT.RouteTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	customRtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(customRtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	got := drainIGWSubnets(t, delSub, 2)
	assert.True(t, got["subnet-priv1"], "joining subnet must receive delete event for main-RT routes")
	assert.Len(t, got, 1)
}

// receiveGateEvent reads one gate/ungate message from sub and returns the
// (subnet, destCidr) it carried.
func receiveGateEvent(t *testing.T, sub *nats.Subscription) (subnetID, destCidr string) {
	t.Helper()
	msg, err := sub.NextMsg(time.Second)
	require.NoError(t, err)
	var evt struct {
		VpcId           string `json:"vpc_id"`
		SubnetId        string `json:"subnet_id"`
		DestinationCidr string `json:"destination_cidr"`
	}
	require.NoError(t, json.Unmarshal(msg.Data, &evt))
	return evt.SubnetId, evt.DestinationCidr
}

func drainGateSubnets(t *testing.T, sub *nats.Subscription, limit int) map[string]string {
	t.Helper()
	got := map[string]string{}
	for range limit {
		msg, err := sub.NextMsg(200 * time.Millisecond)
		if err != nil {
			break
		}
		var evt struct {
			SubnetId        string `json:"subnet_id"`
			DestinationCidr string `json:"destination_cidr"`
		}
		require.NoError(t, json.Unmarshal(msg.Data, &evt))
		got[evt.SubnetId] = evt.DestinationCidr
	}
	return got
}

// TestCreateRoute_IGW_PublishesUngateForAssociatedSubnets: adding an IGW default route
// to a table must flip associated subnets to "egress allowed" (ungate).
func TestCreateRoute_IGW_PublishesUngateForAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	// Drain Associate-time gate event (gate, no egress yet).
	for {
		if _, err := ungate.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, dest := receiveGateEvent(t, ungate)
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "0.0.0.0/0", dest)
}

// TestDeleteRoute_IGW_PublishesGateForAssociatedSubnets: removing the IGW
// default route from a table flips associated subnets back to "no egress"
// so the subscriber must reinstall the drop policy.
func TestDeleteRoute_IGW_PublishesGateForAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	for {
		if _, err := gate.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}

	_, err = svc.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, dest := receiveGateEvent(t, gate)
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "0.0.0.0/0", dest)
}

// TestAssociateRouteTable_NoDefaultRoute_PublishesGate: joining a table that
// has no 0.0.0.0/0 route must publish a gate event so the subnet is dropped.
func TestAssociateRouteTable_NoDefaultRoute_PublishesGate(t *testing.T) {
	svc := setupTestService(t)
	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, dest := receiveGateEvent(t, gate)
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "0.0.0.0/0", dest)
}

// TestAssociateRouteTable_HasIGWRoute_PublishesUngate: joining a table that
// already carries 0.0.0.0/0 -> IGW must publish ungate so any pre-existing
// drop policy is removed.
func TestAssociateRouteTable_HasIGWRoute_PublishesUngate(t *testing.T) {
	svc := setupTestService(t)
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	subnetID, dest := receiveGateEvent(t, ungate)
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "0.0.0.0/0", dest)
}

// TestDisassociateRouteTable_MainHasNoEgress_PublishesGate: leaving an
// egress-carrying table for a main RT without 0.0.0.0/0 must gate.
func TestDisassociateRouteTable_MainHasNoEgress_PublishesGate(t *testing.T) {
	svc := setupTestService(t)
	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()

	// Main RT with no 0.0.0.0/0.
	_, err = svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)
	customRtbID := createTestRtb(t, svc)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(customRtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(customRtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	for {
		if _, err := gate.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: assocOut.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	subnetID, dest := receiveGateEvent(t, gate)
	assert.Equal(t, "subnet-priv1", subnetID)
	assert.Equal(t, "0.0.0.0/0", dest)
}

// TestCreateRoute_NonDefaultPrefix_NoGateEvent: only 0.0.0.0/0 routes drive
// gate decisions today (subnet-level scoping is what AWS RTs gate on).
func TestCreateRoute_NonDefaultPrefix_NoGateEvent(t *testing.T) {
	svc := setupTestService(t)
	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)
	// Drain the Associate-time gate event.
	for {
		if _, err := gate.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}

	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("8.8.8.8/32"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = gate.NextMsg(200 * time.Millisecond)
	assert.Error(t, err, "non-default prefix must not emit a gate event")
	_, err = ungate.NextMsg(200 * time.Millisecond)
	assert.Error(t, err, "non-default prefix must not emit an ungate event")
}

// TestCreateRoute_IGW_OnMainRT_FansOutGateToImplicitSubnets: when the main
// RT acquires an IGW default route, implicit-main subnets receive ungate.
func TestCreateRoute_IGW_OnMainRT_FansOutUngateToImplicitSubnets(t *testing.T) {
	svc := setupTestService(t)
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	mainRT, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT.RouteTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	got := drainGateSubnets(t, ungate, 3)
	assert.Equal(t, "0.0.0.0/0", got["subnet-test1"])
	assert.Equal(t, "0.0.0.0/0", got["subnet-priv1"])
}

func TestDisassociateRouteTable_RestoresMainRTRoutesForDepartingSubnet(t *testing.T) {
	svc := setupTestService(t)
	addSub, err := svc.natsConn.SubscribeSync("vpc.add-igw-route")
	require.NoError(t, err)
	defer func() { _ = addSub.Unsubscribe() }()

	mainRT, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(mainRT.RouteTableId),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	customRtbID := createTestRtb(t, svc)
	assocOut, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(customRtbID),
		SubnetId:     aws.String("subnet-priv1"),
	}, testAccountID)
	require.NoError(t, err)

	// Drain everything published during setup so the Disassociate assertions
	// only see post-disassociate events.
	for {
		if _, err := addSub.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}

	_, err = svc.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{
		AssociationId: assocOut.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	got := drainIGWSubnets(t, addSub, 2)
	assert.True(t, got["subnet-priv1"], "departing subnet must receive add event for main-RT routes")
	assert.Len(t, got, 1)
}

// TestPublishGateDecisionsForVPC_GatesAllSubnetsWhenNoEgress: with a main RT
// carrying only the local route, every subnet in the VPC must receive a gate
// event. Exercises the IGW attach/detach fan-out hook.
func TestPublishGateDecisionsForVPC_GatesAllSubnetsWhenNoEgress(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()

	svc.PublishGateDecisionsForVPC(testAccountID, "vpc-test1", "0.0.0.0/0")

	got := drainGateSubnets(t, gate, 4)
	assert.Equal(t, "0.0.0.0/0", got["subnet-test1"])
	assert.Equal(t, "0.0.0.0/0", got["subnet-priv1"])
	assert.Len(t, got, 2)
}

// TestPublishGateDecisionsForVPC_UngatesSubnetWithIGWRoute: a subnet whose
// effective RT carries 0.0.0.0/0 -> IGW must receive ungate; the other
// (implicit-main, no egress) subnet still receives gate.
func TestPublishGateDecisionsForVPC_UngatesSubnetWithIGWRoute(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateRouteTableForVPC("vpc-test1", "10.0.0.0/16", testAccountID, true, "")
	require.NoError(t, err)

	// Custom RT with 0.0.0.0/0 -> IGW, associated to subnet-test1.
	rtbID := createTestRtb(t, svc)
	_, err = svc.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID),
		SubnetId:     aws.String("subnet-test1"),
	}, testAccountID)
	require.NoError(t, err)

	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	svc.PublishGateDecisionsForVPC(testAccountID, "vpc-test1", "0.0.0.0/0")

	gateGot := drainGateSubnets(t, gate, 4)
	ungateGot := drainGateSubnets(t, ungate, 4)
	assert.Equal(t, "0.0.0.0/0", ungateGot["subnet-test1"], "subnet-test1 has IGW route → ungate")
	assert.Equal(t, "0.0.0.0/0", gateGot["subnet-priv1"], "subnet-priv1 implicit-main no egress → gate")
	assert.Len(t, ungateGot, 1)
	assert.Len(t, gateGot, 1)
}

// TestEffectiveRouteTable_DeterministicWithDuplicateMain seeds two IsMain=true RTs
// for the same VPC and asserts effectiveRouteTable deterministically picks the one
// with more routes (the "real" RT) over the orphan with only the implicit local route.
func TestEffectiveRouteTable_DeterministicWithDuplicateMain(t *testing.T) {
	svc := setupTestService(t)
	now := time.Now()

	orphan := RouteTableRecord{
		RouteTableId: "rtb-zzzorphan",
		VpcId:        "vpc-test1",
		AccountID:    testAccountID,
		IsMain:       true,
		Routes:       []RouteRecord{{DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local", State: "active", Origin: "CreateRouteTable"}},
		CreatedAt:    now.Add(time.Second), // later, would lose tiebreak even with same route count
	}
	real_ := RouteTableRecord{
		RouteTableId: "rtb-aaareal",
		VpcId:        "vpc-test1",
		AccountID:    testAccountID,
		IsMain:       true,
		Routes: []RouteRecord{
			{DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local", State: "active", Origin: "CreateRouteTable"},
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-test1", State: "active", Origin: "CreateRoute"},
		},
		CreatedAt: now,
	}
	require.NoError(t, svc.putRouteTable(testAccountID, &orphan))
	require.NoError(t, svc.putRouteTable(testAccountID, &real_))

	rt, err := svc.effectiveRouteTable(testAccountID, "vpc-test1", "subnet-priv1")
	require.NoError(t, err)
	require.NotNil(t, rt)
	assert.Equal(t, "rtb-aaareal", rt.RouteTableId, "must prefer main RT with more routes")

	main, err := svc.mainRouteTable(testAccountID, "vpc-test1")
	require.NoError(t, err)
	require.NotNil(t, main)
	assert.Equal(t, "rtb-aaareal", main.RouteTableId, "mainRouteTable must use same tiebreak")
}

// TestPreferMain_RouteCountWinsOverCreatedAt confirms route-count is the
// primary tiebreaker — a later-created RT with more routes beats an older
// orphan with only the implicit local route.
func TestPreferMain_RouteCountWinsOverCreatedAt(t *testing.T) {
	older := &RouteTableRecord{RouteTableId: "rtb-a", CreatedAt: time.Unix(100, 0), Routes: []RouteRecord{{}}}
	newer := &RouteTableRecord{RouteTableId: "rtb-b", CreatedAt: time.Unix(200, 0), Routes: []RouteRecord{{}, {}}}
	assert.True(t, preferMain(older, newer), "more routes wins regardless of CreatedAt")
	assert.False(t, preferMain(newer, older), "asymmetric — older with fewer routes loses")
}

// TestPreferMain_CreatedAtTiebreak: equal route counts → oldest CreatedAt
// wins (original record beats the late dup).
func TestPreferMain_CreatedAtTiebreak(t *testing.T) {
	older := &RouteTableRecord{RouteTableId: "rtb-b", CreatedAt: time.Unix(100, 0), Routes: []RouteRecord{{}}}
	newer := &RouteTableRecord{RouteTableId: "rtb-a", CreatedAt: time.Unix(200, 0), Routes: []RouteRecord{{}}}
	assert.True(t, preferMain(newer, older), "older CreatedAt wins on equal route count")
	assert.False(t, preferMain(older, newer), "asymmetric — newer loses")
}

// TestPublishGateDecisionsForVPC_EmptyVPCNoOp: VPC with zero subnets must
// silently emit no events.
func TestPublishGateDecisionsForVPC_EmptyVPCNoOp(t *testing.T) {
	svc := setupTestService(t)
	gate, err := svc.natsConn.SubscribeSync("vpc.gate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = gate.Unsubscribe() }()
	ungate, err := svc.natsConn.SubscribeSync("vpc.ungate-subnet-egress")
	require.NoError(t, err)
	defer func() { _ = ungate.Unsubscribe() }()

	svc.PublishGateDecisionsForVPC(testAccountID, "vpc-empty", "0.0.0.0/0")

	_, err = gate.NextMsg(150 * time.Millisecond)
	assert.Error(t, err, "expected no gate event")
	_, err = ungate.NextMsg(150 * time.Millisecond)
	assert.Error(t, err, "expected no ungate event")
}
