package routetable

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePeering serves route targets and active states from fixed maps.
type fakePeering struct {
	targets map[string]*ec2vpcpeering.RouteTarget
	active  map[string]bool
}

var _ PeeringLookup = (*fakePeering)(nil)

func (f *fakePeering) RouteTarget(_ context.Context, _, id string) (*ec2vpcpeering.RouteTarget, error) {
	return f.targets[id], nil
}

func (f *fakePeering) ActiveConnections(_ context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = f.active[id]
	}
	return out, nil
}

func peeringService(t *testing.T) (*RouteTableServiceImpl, *fakePeering, string) {
	t.Helper()
	svc := setupTestService(t)
	p := &fakePeering{
		targets: map[string]*ec2vpcpeering.RouteTarget{
			"pcx-ours":  {RequesterVpcID: "vpc-test1", AccepterVpcID: "vpc-peer"},
			"pcx-other": {RequesterVpcID: "vpc-x", AccepterVpcID: "vpc-y"},
		},
		active: map[string]bool{},
	}
	svc.SetPeeringLookup(p)
	return svc, p, createTestRtb(t, svc)
}

func peeringRouteState(t *testing.T, svc *RouteTableServiceImpl, rtbID, dest string) *ec2.Route {
	t.Helper()
	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{RouteTableIds: aws.StringSlice([]string{rtbID})}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	for _, r := range out.RouteTables[0].Routes {
		if aws.StringValue(r.DestinationCidrBlock) == dest {
			return r
		}
	}
	t.Fatalf("no route to %s", dest)
	return nil
}

func TestCreateRoute_PeeringTargetStateFollowsConnection(t *testing.T) {
	t.Parallel()
	svc, p, rtbID := peeringService(t)

	_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("10.9.0.0/16"), VpcPeeringConnectionId: aws.String("pcx-ours"),
	}, testAccountID)
	require.NoError(t, err)

	r := peeringRouteState(t, svc, rtbID, "10.9.0.0/16")
	assert.Equal(t, "pcx-ours", aws.StringValue(r.VpcPeeringConnectionId))
	assert.Equal(t, ec2.RouteStateBlackhole, aws.StringValue(r.State), "a route to a connection not yet active is blackhole")
	assert.Nil(t, r.GatewayId)

	p.active["pcx-ours"] = true
	assert.Equal(t, ec2.RouteStateActive, aws.StringValue(peeringRouteState(t, svc, rtbID, "10.9.0.0/16").State))

	// The state is derived before filtering, so route.state sees it.
	for state, want := range map[string]int{ec2.RouteStateActive: 1, ec2.RouteStateBlackhole: 0} {
		out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{Filters: []*ec2.Filter{
			{Name: aws.String("route.state"), Values: aws.StringSlice([]string{state})},
			{Name: aws.String("route.vpc-peering-connection-id"), Values: aws.StringSlice([]string{"pcx-ours"})},
		}}, testAccountID)
		require.NoError(t, err)
		assert.Len(t, out.RouteTables, want, state)
	}

	delete(p.active, "pcx-ours")
	assert.Equal(t, ec2.RouteStateBlackhole, aws.StringValue(peeringRouteState(t, svc, rtbID, "10.9.0.0/16").State))
}

func TestCreateRoute_PeeringTargetRefusals(t *testing.T) {
	t.Parallel()
	svc, _, rtbID := peeringService(t)

	_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("10.9.0.0/16"), VpcPeeringConnectionId: aws.String("pcx-missing"),
	}, testAccountID)
	testutil.RequireAWSError(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The vpcPeeringConnection ID 'pcx-missing' does not exist")

	_, err = svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("10.9.0.0/16"), VpcPeeringConnectionId: aws.String("pcx-other"),
	}, testAccountID)
	testutil.RequireAWSError(t, err, awserrors.ErrorInvalidParameterValue,
		"route table "+rtbID+" and network gateway pcx-other belong to different networks")

	_, err = svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("10.0.5.0/24"), VpcPeeringConnectionId: aws.String("pcx-ours"),
	}, testAccountID)
	require.Error(t, err, "a destination inside the VPC keeps the local-route rule")
}

func TestReplaceRoute_ToAndFromPeering(t *testing.T) {
	t.Parallel()
	svc, p, rtbID := peeringService(t)
	p.active["pcx-ours"] = true

	_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), VpcPeeringConnectionId: aws.String("pcx-ours"),
	}, testAccountID)
	require.NoError(t, err)
	r := peeringRouteState(t, svc, rtbID, "0.0.0.0/0")
	assert.Nil(t, r.GatewayId)
	assert.Equal(t, "pcx-ours", aws.StringValue(r.VpcPeeringConnectionId))
	assert.Equal(t, ec2.RouteStateActive, aws.StringValue(r.State))

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), VpcPeeringConnectionId: aws.String("pcx-missing"),
	}, testAccountID)
	testutil.RequireAWSError(t, err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The vpcPeeringConnection ID 'pcx-missing' does not exist")

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{
		RouteTableId: aws.String(rtbID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-test1"),
	}, testAccountID)
	require.NoError(t, err)
	r = peeringRouteState(t, svc, rtbID, "0.0.0.0/0")
	assert.Equal(t, "igw-test1", aws.StringValue(r.GatewayId))
	assert.Nil(t, r.VpcPeeringConnectionId)
	assert.Equal(t, ec2.RouteStateActive, aws.StringValue(r.State))
}
