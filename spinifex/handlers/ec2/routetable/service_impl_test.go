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

// ackSub wraps an async NATS subscription that auto-responds
// `{"success":true}` to any message carrying a Reply subject. Required for
// topics whose publisher uses utils.RequestEvent (e.g. vpc.add-nat-gateway,
// vpc.add-igw-route) — the synchronous Request blocks until a responder acks.
type ackSub struct {
	sub *nats.Subscription
	ch  chan *nats.Msg
}

func subscribeAck(t *testing.T, nc *nats.Conn, subject string) *ackSub {
	t.Helper()
	ch := make(chan *nats.Msg, 64)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		if m.Reply != "" {
			_ = m.Respond([]byte(`{"success":true}`))
		}
		select {
		case ch <- m:
		default:
			t.Errorf("ackSub buffer full for %s", subject)
		}
	})
	require.NoError(t, err)
	return &ackSub{sub: sub, ch: ch}
}

func (a *ackSub) NextMsg(timeout time.Duration) (*nats.Msg, error) {
	select {
	case m := <-a.ch:
		return m, nil
	case <-time.After(timeout):
		return nil, nats.ErrTimeout
	}
}

func (a *ackSub) Unsubscribe() error { return a.sub.Unsubscribe() }
func (a *ackSub) Subject() string    { return a.sub.Subject }

// receiveNatGWEvent reads one message from sub, decodes, and asserts topic+cidr.
// Returns the decoded public_ip for further assertions.
func receiveNatGWEvent(t *testing.T, sub *ackSub, wantCidr string) (natgwID, publicIp string) {
	t.Helper()
	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err, "expected NAT GW event on %s", sub.Subject())
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

// TestAssociateRouteTable_PublishesNatGatewayEvent covers the terraform flow:
// CreateRouteTable → CreateRoute(NAT GW) → AssociateRouteTable(subnet). CreateRoute
// runs against an empty-association table so no event fires there; Associate must
// emit the event so vpcd installs the SNAT rule for the joining subnet.
func TestAssociateRouteTable_PublishesNatGatewayEvent(t *testing.T) {
	svc := setupTestService(t)
	sub := subscribeAck(t, svc.natsConn, "vpc.add-nat-gateway")
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	// Add NAT GW route BEFORE any subnet is associated (terraform ordering).
	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
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
	sub := subscribeAck(t, svc.natsConn, "vpc.add-nat-gateway")
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
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
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-nat-gateway")
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
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

// TestDeleteRoute_NATGW_PublishesDeleteForAssociatedSubnets covers DeleteRoute
// on a NATGW route: every subnet associated with the table receives a
// vpc.delete-nat-gateway event so the subscriber tears down both the SNAT rule
// and the per-subnet egress policy (mirror of the IGW DeleteRoute path).
func TestDeleteRoute_NATGW_PublishesDeleteForAssociatedSubnets(t *testing.T) {
	svc := setupTestService(t)
	addSub := subscribeAck(t, svc.natsConn, "vpc.add-nat-gateway")
	defer func() { _ = addSub.Unsubscribe() }()
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-nat-gateway")
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
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
	addSub := subscribeAck(t, svc.natsConn, "vpc.add-nat-gateway")
	defer func() { _ = addSub.Unsubscribe() }()
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-nat-gateway")
	defer func() { _ = delSub.Unsubscribe() }()

	oldRtb := createTestRtb(t, svc)
	newRtb := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
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

func receiveIGWRouteEvent(t *testing.T, sub *ackSub, wantCidr string) (subnetID, igwID, destCidr string) {
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
	sub := subscribeAck(t, svc.natsConn, "vpc.add-igw-route")
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	// Associate subnet FIRST, then CreateRoute — exercises the "route after
	// association" leg.
	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
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

// TestAssociateRouteTable_PublishesAddIGWRouteForExistingRoute covers the
// terraform ordering: CreateRoute(IGW) → AssociateRouteTable. CreateRoute runs
// against an empty-association table so no event fires; Associate must emit
// the event so the subscriber installs the policy for the joining subnet.
func TestAssociateRouteTable_PublishesAddIGWRouteForExistingRoute(t *testing.T) {
	svc := setupTestService(t)
	sub := subscribeAck(t, svc.natsConn, "vpc.add-igw-route")
	defer func() { _ = sub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)

	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
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
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-igw-route")
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err := svc.AssociateRouteTable(&ec2.AssociateRouteTableInput{
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
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-igw-route")
	defer func() { _ = delSub.Unsubscribe() }()

	rtbID := createTestRtb(t, svc)
	_, err := svc.CreateRoute(&ec2.CreateRouteInput{
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
func drainIGWSubnets(t *testing.T, sub *ackSub, count int) map[string]bool {
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

// TestCreateRoute_IGW_OnMainRT_FansOutToImplicitSubnets covers the primary
// publish-gap fixed in this change: CreateRoute(IGW) on a VPC's main RT must
// emit one vpc.add-igw-route event per subnet that has no explicit non-main
// RT association (AWS implicit-main semantics).
func TestCreateRoute_IGW_OnMainRT_FansOutToImplicitSubnets(t *testing.T) {
	svc := setupTestService(t)
	sub := subscribeAck(t, svc.natsConn, "vpc.add-igw-route")
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
	sub := subscribeAck(t, svc.natsConn, "vpc.add-igw-route")
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

// TestAssociateRouteTable_RemovesMainRTRoutesForJoiningSubnet covers the
// symmetric fix: when a subnet leaves implicit main-RT membership for an
// explicit non-main RT, the main RT's per-subnet rules must be torn down so
// state matches AWS effective-route semantics.
func TestAssociateRouteTable_RemovesMainRTRoutesForJoiningSubnet(t *testing.T) {
	svc := setupTestService(t)
	delSub := subscribeAck(t, svc.natsConn, "vpc.delete-igw-route")
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

// TestDisassociateRouteTable_RestoresMainRTRoutesForDepartingSubnet covers
// the symmetric add-back: when a subnet leaves an explicit RT and falls back
// to implicit main-RT membership, the main RT's per-subnet rules must be
// re-installed.
func TestDisassociateRouteTable_RestoresMainRTRoutesForDepartingSubnet(t *testing.T) {
	svc := setupTestService(t)
	addSub := subscribeAck(t, svc.natsConn, "vpc.add-igw-route")
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
