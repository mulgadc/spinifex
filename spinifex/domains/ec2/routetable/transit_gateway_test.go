package routetable

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTGW   = "tgw-0123456789abcdef0"
	otherTGW  = "tgw-0fedcba9876543210"
	tgwRouted = "10.20.0.0/16"
)

// fakeTGWs answers route targets and route states from fixed maps keyed by
// gateway and VPC.
type fakeTGWs struct {
	routable map[string]bool
	active   map[string]bool
}

var _ TransitGatewayLookup = (*fakeTGWs)(nil)

func (f *fakeTGWs) RouteTarget(_ context.Context, _, tgwID, vpcID string) (bool, error) {
	return f.routable[tgwID+"/"+vpcID], nil
}

func (f *fakeTGWs) RouteActive(_ context.Context, _, tgwID, vpcID string) (bool, error) {
	return f.active[tgwID+"/"+vpcID], nil
}

// fakeAdmission answers with ok and err, and counts calls to done.
type fakeAdmission struct {
	ok    bool
	err   error
	calls atomic.Int32
	dones atomic.Int32
}

func (a *fakeAdmission) admit(context.Context) (func(), bool, error) {
	a.calls.Add(1)
	return func() { a.dones.Add(1) }, a.ok, a.err
}

func tgwService(t *testing.T) (*RouteTableServiceImpl, *fakeTGWs, *fakeAdmission, string) {
	t.Helper()
	svc := setupTestService(t)
	l := &fakeTGWs{
		routable: map[string]bool{testTGW + "/vpc-test1": true, otherTGW + "/vpc-test1": true},
		active:   map[string]bool{},
	}
	a := &fakeAdmission{ok: true}
	svc.SetTransitGatewayLookup(l, a.admit)
	return svc, l, a, createTestRtb(t, svc)
}

func createTGWRoute(svc *RouteTableServiceImpl, rtbID, dest, tgwID string) error {
	_, err := svc.CreateRoute(context.Background(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String(dest), TransitGatewayId: aws.String(tgwID),
	}, testAccountID)
	return err
}

func storedRoutes(t *testing.T, svc *RouteTableServiceImpl, rtbID string) []RouteRecord {
	t.Helper()
	rec, err := svc.getRouteTable(t.Context(), testAccountID, rtbID)
	require.NoError(t, err)
	return rec.Routes
}

func storedRoute(t *testing.T, svc *RouteTableServiceImpl, rtbID, dest string) RouteRecord {
	t.Helper()
	for _, r := range storedRoutes(t, svc, rtbID) {
		if r.DestinationCidrBlock == dest {
			return r
		}
	}
	t.Fatalf("no stored route to %s", dest)
	return RouteRecord{}
}

func TestCreateRoute_TransitGatewayRefusedBelowLevel(t *testing.T) {
	t.Parallel()
	const msg = "Transit Gateways are not available until every node in the formation runs a release that supports them."

	plain := setupTestService(t)
	rtbID := createTestRtb(t, plain)
	testutil.RequireAWSError(t, createTGWRoute(plain, rtbID, tgwRouted, testTGW), awserrors.ErrorUnsupportedOperation, msg)
	assert.Len(t, storedRoutes(t, plain, rtbID), 1, "only the local route")

	svc, _, a, rtbID := tgwService(t)
	a.ok = false
	testutil.RequireAWSError(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW), awserrors.ErrorUnsupportedOperation, msg)
	assert.Len(t, storedRoutes(t, svc, rtbID), 1)
	assert.Equal(t, int32(1), a.dones.Load())

	a.ok, a.err = false, errors.New("level store unavailable")
	testutil.RequireAWSCode(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW), awserrors.ErrorServerInternal)
	assert.Len(t, storedRoutes(t, svc, rtbID), 1)
	assert.Equal(t, int32(2), a.dones.Load())

	_, err := svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("10.0.0.0/16"), TransitGatewayId: aws.String(testTGW),
	}, testAccountID)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServerInternal)
	assert.Equal(t, a.calls.Load(), a.dones.Load(), "every admission is released once")
}

func TestCreateRoute_TransitGatewayTarget(t *testing.T) {
	t.Parallel()
	svc, l, a, rtbID := tgwService(t)

	const unknown = "tgw-0aaaaaaaaaaaaaaaa"
	testutil.RequireAWSError(t, createTGWRoute(svc, rtbID, tgwRouted, unknown),
		awserrors.ErrorInvalidTransitGatewayIDNotFound, "The transitGateway ID '"+unknown+"' does not exist.")
	testutil.RequireAWSError(t, createTGWRoute(svc, rtbID, tgwRouted, "tgw-bogus"),
		awserrors.ErrorInvalidTransitGatewayIDNotFound, "The transitGateway ID 'tgw-bogus' does not exist.")
	assert.Len(t, storedRoutes(t, svc, rtbID), 1)

	err := createTGWRoute(svc, rtbID, "10.0.5.0/24", testTGW)
	testutil.RequireAWSError(t, err, awserrors.ErrorInvalidParameterValue,
		"The destination CIDR block 10.0.5.0/24 is equal to or more specific than one of this VPC's CIDR blocks. This route can target only an interface or an instance.")

	require.NoError(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW))
	r := storedRoute(t, svc, rtbID, tgwRouted)
	assert.Equal(t, testTGW, r.TransitGatewayId)
	assert.Equal(t, ec2.RouteOriginCreateRoute, r.Origin)
	assert.Empty(t, r.GatewayId)
	assert.Empty(t, r.State, "the state is derived when described")
	assert.Equal(t, a.calls.Load(), a.dones.Load(), "every admission is released once")

	l.routable[testTGW+"/vpc-test1"] = false
	testutil.RequireAWSError(t, createTGWRoute(svc, rtbID, "10.30.0.0/16", testTGW),
		awserrors.ErrorInvalidTransitGatewayIDNotFound, "The transitGateway ID '"+testTGW+"' does not exist.")
}

func describedRoute(t *testing.T, svc *RouteTableServiceImpl, rtbID, dest string) *ec2.Route {
	t.Helper()
	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{RouteTableIds: aws.StringSlice([]string{rtbID})}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == dest {
			return r
		}
	}
	t.Fatalf("no described route to %s", dest)
	return nil
}

func TestDescribeRouteTables_TransitGatewayRouteStateAndFilter(t *testing.T) {
	t.Parallel()
	svc, l, _, rtbID := tgwService(t)
	other := createTestRtb(t, svc)
	require.NoError(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW))
	require.NoError(t, createTGWRoute(svc, other, tgwRouted, otherTGW))

	r := describedRoute(t, svc, rtbID, tgwRouted)
	assert.Equal(t, testTGW, aws.StringValue(r.TransitGatewayId))
	assert.Equal(t, ec2.RouteStateBlackhole, aws.StringValue(r.State), "no available attachment")
	assert.Nil(t, r.GatewayId)

	l.active[testTGW+"/vpc-test1"] = true
	assert.Equal(t, ec2.RouteStateActive, aws.StringValue(describedRoute(t, svc, rtbID, tgwRouted).State))

	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.transit-gateway-id"), Values: aws.StringSlice([]string{testTGW})}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	assert.Equal(t, rtbID, aws.StringValue(out.RouteTables[0].RouteTableId))

	out, err = svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{{Name: aws.String("route.state"), Values: aws.StringSlice([]string{ec2.RouteStateBlackhole})}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1, "route.state matches the derived state")
	assert.Equal(t, other, aws.StringValue(out.RouteTables[0].RouteTableId))
}

func TestReplaceRoute_ToAndFromTransitGateway(t *testing.T) {
	t.Parallel()
	svc, _, a, rtbID := tgwService(t)
	_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), TransitGatewayId: aws.String(testTGW),
	}, testAccountID)
	require.NoError(t, err)
	r := storedRoute(t, svc, rtbID, "0.0.0.0/0")
	assert.Equal(t, testTGW, r.TransitGatewayId)
	assert.Empty(t, r.GatewayId)

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	r = storedRoute(t, svc, rtbID, "0.0.0.0/0")
	assert.Empty(t, r.TransitGatewayId)
	assert.Equal(t, "igw-test1", r.GatewayId)
	assert.Equal(t, a.calls.Load(), a.dones.Load())
}

func TestCountTransitGatewayRoutes(t *testing.T) {
	t.Parallel()
	_, _, empty := testutil.StartTestJetStream(t)
	n, err := CountTransitGatewayRoutes(t.Context(), empty)
	require.NoError(t, err)
	assert.Zero(t, n, "a missing bucket holds no route")

	svc, _, _, rtbID := tgwService(t)
	js := testutil.NewJetStream(t, svc.natsConn)
	n, err = CountTransitGatewayRoutes(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, n)

	require.NoError(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW))
	require.NoError(t, createTGWRoute(svc, rtbID, "10.21.0.0/16", otherTGW))
	n, err = CountTransitGatewayRoutes(t.Context(), js)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// olderRouteRecord and olderRouteTable are the records as a daemon before
// Transit Gateway routes encodes them.
type olderRouteRecord struct {
	DestinationCidrBlock    string `json:"destination_cidr_block"`
	GatewayId               string `json:"gateway_id,omitempty"`
	NatGatewayId            string `json:"nat_gateway_id,omitempty"`
	VpcPeeringConnectionId  string `json:"vpc_peering_connection_id,omitempty"`
	DestinationPrefixListId string `json:"destination_prefix_list_id,omitempty"`
	State                   string `json:"state"`
	Origin                  string `json:"origin"`
}

type olderRouteTable struct {
	RouteTableId string              `json:"route_table_id"`
	VpcId        string              `json:"vpc_id"`
	AccountID    string              `json:"account_id"`
	IsMain       bool                `json:"is_main"`
	Routes       []olderRouteRecord  `json:"routes"`
	Associations []AssociationRecord `json:"associations"`
	Tags         map[string]string   `json:"tags"`
	CreatedAt    time.Time           `json:"created_at"`
}

// An older writer's rewrite drops the route's gateway, so the drain must
// count such routes before any older daemon returns.
func TestTransitGatewayRoute_OlderWriterDropsIt(t *testing.T) {
	t.Parallel()
	svc, _, _, rtbID := tgwService(t)
	js := testutil.NewJetStream(t, svc.natsConn)
	require.NoError(t, createTGWRoute(svc, rtbID, tgwRouted, testTGW))
	n, err := CountTransitGatewayRoutes(t.Context(), js)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	key := kvstore.AccountKey(testAccountID, rtbID)
	entry, err := svc.rtbKV.Get(t.Context(), key)
	require.NoError(t, err)
	var older olderRouteTable
	require.NoError(t, json.Unmarshal(entry.Value(), &older))
	data, err := json.Marshal(older)
	require.NoError(t, err)
	_, err = svc.rtbKV.Update(t.Context(), key, data, entry.Revision())
	require.NoError(t, err)

	assert.Empty(t, storedRoute(t, svc, rtbID, tgwRouted).TransitGatewayId)
	n, err = CountTransitGatewayRoutes(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, n)
}
