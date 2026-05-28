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

func TestRouteManager_AddSubnetEgress_InstallsScopedPolicy(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	p := policies[0]
	assert.Equal(t, "reroute", p.Action)
	assert.Equal(t, SubnetEgressPriorityIGW, p.Priority)
	require.NotNil(t, p.Nexthop)
	assert.Equal(t, "192.168.1.1", *p.Nexthop)
	assert.Contains(t, p.Match, topology.SubnetRouterPort("subnet-pub"))
	assert.Contains(t, p.Match, "ip4.dst == 0.0.0.0/0")
	assert.Equal(t, "subnet-pub", p.ExternalIDs["spinifex:subnet"])
	assert.Equal(t, "gw-vpc-1", p.ExternalIDs["spinifex:output_port"])
}

func TestRouteManager_AddSubnetEgress_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", spec))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 1)
}

func TestRouteManager_AddSubnetEgress_DriftReplacesNexthop(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", spec))
	spec.Nexthop = "192.168.1.254"
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", spec))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, "192.168.1.254", *policies[0].Nexthop)
}

func TestRouteManager_AddSubnetEgress_PerSubnetSeparate(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	base := SubnetEgressSpec{
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}
	a := base
	a.SubnetID = "subnet-a"
	b := base
	b.SubnetID = "subnet-b"
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", a))
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", b))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 2, "per-subnet egress policies must coexist on the same VPC router")
}

func TestRouteManager_DeleteSubnetEgress_RemovesPolicy(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     prefix,
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}))
	require.NoError(t, rm.DeleteSubnetEgress(ctx, "vpc-1", "subnet-pub", prefix))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

func TestRouteManager_DeleteSubnetEgress_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.DeleteSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0")))
}
