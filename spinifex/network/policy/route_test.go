package policy

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func findRoute(m *mock.Client, prefix string) *nbdb.LogicalRouterStaticRoute {
	for _, r := range m.StaticRoutes {
		if r.IPPrefix == prefix {
			return r
		}
	}
	return nil
}

func countRoutes(m *mock.Client, prefix string) int {
	n := 0
	for _, r := range m.StaticRoutes {
		if r.IPPrefix == prefix {
			n++
		}
	}
	return n
}

func TestRouteManager_AddDefaultRoute_WritesOutputPort(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-1", "192.168.1.1", topology.GatewayRouterPort("vpc-1")))

	got := findRoute(m, "0.0.0.0/0")
	require.NotNil(t, got)
	assert.Equal(t, "192.168.1.1", got.Nexthop)
	require.NotNil(t, got.OutputPort)
	assert.Equal(t, "gw-vpc-1", *got.OutputPort)
}

func TestRouteManager_AddStaticRoute_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := RouteSpec{
		Prefix:  netip.MustParsePrefix("10.42.0.0/24"),
		Nexthop: "10.0.0.254",
	}
	require.NoError(t, rm.AddStaticRoute(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddStaticRoute(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddStaticRoute(ctx, "vpc-1", spec))

	assert.Equal(t, 1, countRoutes(m, "10.42.0.0/24"))
}

func TestRouteManager_AddStaticRoute_DriftReplacesRow(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-1", "192.168.1.1", "gw-vpc-1"))
	// Drift the nexthop.
	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-1", "192.168.1.254", "gw-vpc-1"))

	assert.Equal(t, 1, countRoutes(m, "0.0.0.0/0"))
	got := findRoute(m, "0.0.0.0/0")
	require.NotNil(t, got)
	assert.Equal(t, "192.168.1.254", got.Nexthop)
}

func TestRouteManager_DeleteDefaultRoute_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.DeleteDefaultRoute(ctx, "vpc-1"))
}

func TestRouteManager_DeleteStaticRoute_RemovesRow(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("10.42.0.0/24")
	require.NoError(t, rm.AddStaticRoute(ctx, "vpc-1", RouteSpec{Prefix: prefix, Nexthop: "10.0.0.254"}))
	require.NotNil(t, findRoute(m, "10.42.0.0/24"))

	require.NoError(t, rm.DeleteStaticRoute(ctx, "vpc-1", prefix))
	assert.Nil(t, findRoute(m, "10.42.0.0/24"))
}
