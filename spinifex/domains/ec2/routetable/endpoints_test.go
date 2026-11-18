package routetable

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const endpointRegion = "ap-southeast-2"

// fakeEndpoints routes each table in routed to its endpoint.
type fakeEndpoints struct{ routed map[string]string }

var _ EndpointLookup = (*fakeEndpoints)(nil)

func (f *fakeEndpoints) EffectiveEndpoints(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if vpce, ok := f.routed[id]; ok {
			out[id] = vpce
		}
	}
	return out, nil
}

func endpointService(t *testing.T) (*RouteTableServiceImpl, string, string) {
	t.Helper()
	svc := setupTestService(t)
	svc.config = &config.Config{Region: endpointRegion}
	routed, plain := createTestRtb(t, svc), createTestRtb(t, svc)
	svc.SetEndpointLookup(&fakeEndpoints{routed: map[string]string{routed: "vpce-0a000000000000001"}})
	return svc, routed, plain
}

func requireAWSError(t *testing.T, err error, code, msg string) {
	t.Helper()
	gotCode, gotMsg, ok := awserrors.ResolveErrorDetail(err)
	require.Truef(t, ok, "not an AWS error: %v", err)
	assert.Equal(t, code, gotCode)
	assert.Equal(t, msg, gotMsg)
}

// An effective endpoint association renders as AWS's derived route, and the
// prefix-list and gateway filters find it.
func TestDescribeRouteTables_DerivesEndpointRoute(t *testing.T) {
	svc, routed, plain := endpointService(t)
	pl := prefixlist.ID(endpointRegion)

	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{RouteTableIds: aws.StringSlice([]string{routed})}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	var endpointRoutes []*ec2.Route
	for _, r := range out.RouteTables[0].Routes {
		if r.DestinationPrefixListId != nil {
			endpointRoutes = append(endpointRoutes, r)
		}
	}
	assert.Equal(t, []*ec2.Route{{
		DestinationPrefixListId: aws.String(pl),
		GatewayId:               aws.String("vpce-0a000000000000001"),
		Origin:                  aws.String(ec2.RouteOriginCreateRoute),
		State:                   aws.String(ec2.RouteStateActive),
	}}, endpointRoutes)

	ids := func(name, value string) []string {
		out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
			Filters: []*ec2.Filter{{Name: aws.String(name), Values: aws.StringSlice([]string{value})}},
		}, testAccountID)
		require.NoError(t, err)
		var got []string
		for _, rt := range out.RouteTables {
			got = append(got, aws.StringValue(rt.RouteTableId))
		}
		return got
	}
	assert.Equal(t, []string{routed}, ids("route.destination-prefix-list-id", pl))
	assert.Equal(t, []string{routed}, ids("route.gateway-id", "vpce-0a000000000000001"))
	assert.Subset(t, ids("route.gateway-id", "local"), []string{routed, plain}, "the derived route leaves the local route matching")
}

// The derived route is the endpoint's: routes naming the S3 list are refused
// with AWS's answers and leave the table unchanged.
func TestRouteMutations_RefuseEndpointRoutes(t *testing.T) {
	svc, routed, plain := endpointService(t)
	pl := aws.String(prefixlist.ID(endpointRegion))

	_, err := svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{RouteTableId: aws.String(plain), DestinationPrefixListId: pl, VpcEndpointId: aws.String("vpce-0a000000000000001")}, testAccountID)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, "Cannot create or replace a prefix list route targeting a VPC Endpoint.")

	_, err = svc.CreateRoute(t.Context(), &ec2.CreateRouteInput{RouteTableId: aws.String(plain), DestinationPrefixListId: pl, GatewayId: aws.String("vpce-0a000000000000001")}, testAccountID)
	requireAWSError(t, err, awserrors.ErrorInvalidGatewayIDNotFound, "The gateway ID 'vpce-0a000000000000001' does not exist")

	_, err = svc.ReplaceRoute(t.Context(), &ec2.ReplaceRouteInput{RouteTableId: aws.String(routed), DestinationPrefixListId: pl, GatewayId: aws.String("vpce-0a000000000000002")}, testAccountID)
	requireAWSError(t, err, awserrors.ErrorInvalidGatewayIDNotFound, "The gateway ID 'vpce-0a000000000000002' does not exist")

	_, err = svc.DeleteRoute(t.Context(), &ec2.DeleteRouteInput{RouteTableId: aws.String(routed), DestinationPrefixListId: pl}, testAccountID)
	requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, "cannot remove VPC endpoint route vpce-0a000000000000001 in route table "+routed)

	_, err = svc.DeleteRoute(t.Context(), &ec2.DeleteRouteInput{RouteTableId: aws.String(plain), DestinationPrefixListId: pl}, testAccountID)
	requireAWSError(t, err, awserrors.ErrorInvalidRouteNotFound, "no route with destination-prefix-list-id "+*pl+" in route table "+plain)

	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{RouteTableIds: aws.StringSlice([]string{plain})}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	assert.Len(t, out.RouteTables[0].Routes, 1, "only the local route remains")
}

func TestRouteTableVpc(t *testing.T) {
	svc, routed, _ := endpointService(t)

	vpc, err := svc.RouteTableVpc(t.Context(), testAccountID, routed)
	require.NoError(t, err)
	assert.Equal(t, "vpc-test1", vpc)

	for _, id := range []string{"rtb-0missing00000000", "rtb-bad id"} {
		vpc, err = svc.RouteTableVpc(t.Context(), testAccountID, id)
		require.NoError(t, err)
		assert.Empty(t, vpc, id)
	}
	vpc, err = svc.RouteTableVpc(t.Context(), "210987654321", routed)
	require.NoError(t, err)
	assert.Empty(t, vpc, "another account's table does not exist for the caller")
}
