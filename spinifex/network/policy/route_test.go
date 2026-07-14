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

// Every VPC router carries 0.0.0.0/0 — a drift replace on one router must
// never delete another router's default route (regression: unscoped prefix
// match grabbed the first 0.0.0.0/0 row DB-wide and hit a referential
// integrity violation, wedging IGW attach on multi-VPC nodes).
func TestRouteManager_DriftReplace_ScopedToRouter(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedRouter(t, m, "vpc-2")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-1", "192.168.1.1", "gw-vpc-1"))
	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-2", "192.168.1.1", "gw-vpc-2"))

	// Drift vpc-2's nexthop (the nat-mode transit migration path).
	require.NoError(t, rm.AddDefaultRoute(ctx, "vpc-2", "100.127.0.1", "gw-vpc-2"))

	assert.Equal(t, 2, countRoutes(m, "0.0.0.0/0"))
	r1 := m.Routers[topology.VPCRouter("vpc-1")]
	require.Len(t, r1.StaticRoutes, 1)
	assert.Equal(t, "192.168.1.1", m.StaticRoutes[r1.StaticRoutes[0]].Nexthop,
		"vpc-1 default route must survive vpc-2's drift replace")
	r2 := m.Routers[topology.VPCRouter("vpc-2")]
	require.Len(t, r2.StaticRoutes, 1)
	assert.Equal(t, "100.127.0.1", m.StaticRoutes[r2.StaticRoutes[0]].Nexthop)
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

func TestRouteManager_AddSystemInstanceEgress_InstallsScopedPolicy(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", SystemInstanceEgressSpec{
		SubnetID:   "subnet-k3s",
		SrcIP:      netip.MustParseAddr("172.31.4.10"),
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	p := policies[0]
	assert.Equal(t, "reroute", p.Action)
	assert.Equal(t, SystemInstanceEgressPriority, p.Priority)
	require.NotNil(t, p.Nexthop)
	assert.Equal(t, "192.168.1.1", *p.Nexthop)
	// Scoped to the single instance /32 source so peers in the subnet are untouched.
	assert.Contains(t, p.Match, "ip4.src == 172.31.4.10/32")
	assert.Contains(t, p.Match, topology.SubnetRouterPort("subnet-k3s"))
	assert.Equal(t, "172.31.4.10", p.ExternalIDs["spinifex:src_ip"])
	assert.Equal(t, "system-instance-egress", p.ExternalIDs["spinifex:role"])
}

func TestRouteManager_AddSystemInstanceEgress_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := SystemInstanceEgressSpec{
		SubnetID:   "subnet-k3s",
		SrcIP:      netip.MustParseAddr("172.31.4.10"),
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
	}
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", spec))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 1)
}

func TestRouteManager_AddSystemInstanceEgress_DriftReplacesNexthop(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := SystemInstanceEgressSpec{
		SubnetID:   "subnet-k3s",
		SrcIP:      netip.MustParseAddr("172.31.4.10"),
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
	}
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", spec))
	spec.Nexthop = "192.168.1.254"
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", spec))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, "192.168.1.254", *policies[0].Nexthop)
}

func TestRouteManager_AddSystemInstanceEgress_RejectsMissingFields(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	valid := SystemInstanceEgressSpec{
		SubnetID:   "subnet-k3s",
		SrcIP:      netip.MustParseAddr("172.31.4.10"),
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
	}

	noSubnet := valid
	noSubnet.SubnetID = ""
	assert.Error(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", noSubnet))

	noSrc := valid
	noSrc.SrcIP = netip.Addr{}
	assert.Error(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", noSrc))

	noPort := valid
	noPort.OutputPort = ""
	assert.Error(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", noPort))
}

func TestRouteManager_DeleteSystemInstanceEgress_RemovesAndIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	src := netip.MustParseAddr("172.31.4.10")
	prefix := netip.MustParsePrefix("0.0.0.0/0")
	require.NoError(t, rm.AddSystemInstanceEgress(ctx, "vpc-1", SystemInstanceEgressSpec{
		SubnetID: "subnet-k3s", SrcIP: src, Prefix: prefix, Nexthop: "192.168.1.1", OutputPort: "gw-vpc-1",
	}))

	require.NoError(t, rm.DeleteSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", src, prefix, nil))
	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
	// Idempotent on a missing policy.
	require.NoError(t, rm.DeleteSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", src, prefix, nil))
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
	require.NoError(t, rm.DeleteSubnetEgress(ctx, "vpc-1", "subnet-pub", prefix, nil))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

func TestRouteManager_DeleteSubnetEgress_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.DeleteSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0"), nil))
}

func TestRouteManager_AddSubnetEgress_RejectsMissingFields(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	base := SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}

	cases := map[string]func(s *SubnetEgressSpec){
		"missing SubnetID":   func(s *SubnetEgressSpec) { s.SubnetID = "" },
		"missing OutputPort": func(s *SubnetEgressSpec) { s.OutputPort = "" },
		"zero Priority":      func(s *SubnetEgressSpec) { s.Priority = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			spec := base
			mut(&spec)
			err := rm.AddSubnetEgress(ctx, "vpc-1", spec)
			require.Error(t, err)
		})
	}
}

func TestRouteManager_AddSubnetEgress_PropagatesFindError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	rm := NewRouteManager(m)

	err := rm.AddSubnetEgress(ctx, "vpc-missing", SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:    "192.168.1.1",
		OutputPort: "gw",
		Priority:   SubnetEgressPriorityIGW,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find LR policy")
}

func TestRouteManager_AddSubnetEgress_ReplacesPolicyOnActionDrift(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	router := topology.VPCRouter("vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	match := subnetEgressMatch("subnet-pub", prefix, nil)

	stale := &nbdb.LogicalRouterPolicy{
		UUID:        "drift-uuid",
		Priority:    SubnetEgressPriorityIGW,
		Match:       match,
		Action:      "drop",
		ExternalIDs: map[string]string{"spinifex:output_port": "gw-vpc-1"},
	}
	m.LRPolicies[stale.UUID] = stale
	m.Routers[router].Policies = append(m.Routers[router].Policies, stale.UUID)

	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     prefix,
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, router)
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, "reroute", policies[0].Action)
}

func TestRouteManager_AddSubnetEgress_ReplacesPolicyOnOutputPortDrift(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	router := topology.VPCRouter("vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	match := subnetEgressMatch("subnet-pub", prefix, nil)
	nexthop := "192.168.1.1"

	stale := &nbdb.LogicalRouterPolicy{
		UUID:        "drift-uuid",
		Priority:    SubnetEgressPriorityIGW,
		Match:       match,
		Action:      "reroute",
		Nexthop:     &nexthop,
		ExternalIDs: map[string]string{"spinifex:output_port": "stale-port"},
	}
	m.LRPolicies[stale.UUID] = stale
	m.Routers[router].Policies = append(m.Routers[router].Policies, stale.UUID)

	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:   "subnet-pub",
		Prefix:     prefix,
		Nexthop:    nexthop,
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, router)
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, "gw-vpc-1", policies[0].ExternalIDs["spinifex:output_port"])
}

func TestRouteManager_DeleteSubnetEgress_PropagatesError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	rm := NewRouteManager(m)

	err := rm.DeleteSubnetEgress(ctx, "vpc-missing", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete IGW LR policy")
}

func TestSubnetEgressMatch_AppendsExcludeCIDRs(t *testing.T) {
	prefix := netip.MustParsePrefix("0.0.0.0/0")
	excludes := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/16"),
		netip.MustParsePrefix("172.20.0.0/24"),
	}
	got := subnetEgressMatch("subnet-pub", prefix, excludes)

	assert.Contains(t, got, `inport == "rtr-subnet-pub"`)
	assert.Contains(t, got, "ip4.dst == 0.0.0.0/0")
	assert.Contains(t, got, "ip4.dst != 10.0.0.0/16")
	assert.Contains(t, got, "ip4.dst != 172.20.0.0/24")
}

func TestSubnetEgressMatch_SkipsInvalidExcludes(t *testing.T) {
	got := subnetEgressMatch("subnet-pub", netip.MustParsePrefix("0.0.0.0/0"), []netip.Prefix{{}})
	assert.NotContains(t, got, "ip4.dst !=")
}

func TestRouteManager_AddSubnetEgress_PersistsExcludeInMatch(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	excludes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:     "subnet-pub",
		Prefix:       netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:      "192.168.1.1",
		OutputPort:   "gw-vpc-1",
		Priority:     SubnetEgressPriorityNATGW,
		ExcludeCIDRs: excludes,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Contains(t, policies[0].Match, "ip4.dst != 10.0.0.0/16")
}

func TestRouteManager_AddSubnetEgressDrop_InstallsDropPolicy(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID: "subnet-pub",
		Prefix:   netip.MustParsePrefix("0.0.0.0/0"),
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	p := policies[0]
	assert.Equal(t, "drop", p.Action)
	assert.Equal(t, SubnetEgressPriorityDrop, p.Priority)
	assert.Nil(t, p.Nexthop, "drop policy must not carry a nexthop")
	assert.Contains(t, p.Match, topology.SubnetRouterPort("subnet-pub"))
	assert.Contains(t, p.Match, "ip4.dst == 0.0.0.0/0")
	assert.Equal(t, "subnet-pub", p.ExternalIDs["spinifex:subnet"])
	assert.Equal(t, "drop", p.ExternalIDs["spinifex:gate"])
}

func TestRouteManager_AddSubnetEgressDrop_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	spec := SubnetEgressDropSpec{SubnetID: "subnet-pub", Prefix: netip.MustParsePrefix("0.0.0.0/0")}
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", spec))
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", spec))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 1)
}

func TestRouteManager_AddSubnetEgressDrop_PersistsExcludesInMatch(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	excludes := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("224.0.0.0/4"),
	}
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID:     "subnet-pub",
		Prefix:       netip.MustParsePrefix("0.0.0.0/0"),
		ExcludeCIDRs: excludes,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Contains(t, policies[0].Match, "ip4.dst != 10.0.0.0/16")
	assert.Contains(t, policies[0].Match, "ip4.dst != 169.254.0.0/16")
	assert.Contains(t, policies[0].Match, "ip4.dst != 224.0.0.0/4")
}

func TestRouteManager_AddSubnetEgressDrop_RejectsMissingFields(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	t.Run("missing SubnetID", func(t *testing.T) {
		err := rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{Prefix: netip.MustParsePrefix("0.0.0.0/0")})
		require.Error(t, err)
	})
	t.Run("invalid Prefix", func(t *testing.T) {
		err := rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{SubnetID: "subnet-pub"})
		require.Error(t, err)
	})
}

func TestRouteManager_AddSubnetEgressDrop_ReplacesPolicyOnActionDrift(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	router := topology.VPCRouter("vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	match := subnetEgressMatch("subnet-pub", prefix, nil)
	nexthop := "10.0.0.1"

	stale := &nbdb.LogicalRouterPolicy{
		UUID:     "drift-uuid",
		Priority: SubnetEgressPriorityDrop,
		Match:    match,
		Action:   "reroute",
		Nexthop:  &nexthop,
	}
	m.LRPolicies[stale.UUID] = stale
	m.Routers[router].Policies = append(m.Routers[router].Policies, stale.UUID)

	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID: "subnet-pub",
		Prefix:   prefix,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, router)
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, "drop", policies[0].Action)
	assert.Nil(t, policies[0].Nexthop)
}

func TestRouteManager_DeleteSubnetEgressDrop_RemovesPolicy(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID: "subnet-pub",
		Prefix:   prefix,
	}))
	require.NoError(t, rm.DeleteSubnetEgressDrop(ctx, "vpc-1", "subnet-pub", prefix, nil))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

func TestRouteManager_DeleteSubnetEgressDrop_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	require.NoError(t, rm.DeleteSubnetEgressDrop(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0"), nil))
}

func TestRouteManager_DeleteSubnetEgressDrop_MatchesAddedExcludes(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	excludes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID:     "subnet-pub",
		Prefix:       prefix,
		ExcludeCIDRs: excludes,
	}))
	require.NoError(t, rm.DeleteSubnetEgressDrop(ctx, "vpc-1", "subnet-pub", prefix, excludes))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
}

func TestRouteManager_DropAndRerouteCoexistAtDifferentPriorities(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:   "subnet-a",
		Prefix:     prefix,
		Nexthop:    "192.168.1.1",
		OutputPort: "gw-vpc-1",
		Priority:   SubnetEgressPriorityIGW,
	}))
	require.NoError(t, rm.AddSubnetEgressDrop(ctx, "vpc-1", SubnetEgressDropSpec{
		SubnetID: "subnet-b",
		Prefix:   prefix,
	}))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 2, "different subnets can hold different action policies on the same VPC router")
}

func TestRouteManager_DeleteSubnetEgress_MatchesAddedExcludes(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	rm := NewRouteManager(m)

	prefix := netip.MustParsePrefix("0.0.0.0/0")
	excludes := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	require.NoError(t, rm.AddSubnetEgress(ctx, "vpc-1", SubnetEgressSpec{
		SubnetID:     "subnet-pub",
		Prefix:       prefix,
		Nexthop:      "192.168.1.1",
		OutputPort:   "gw-vpc-1",
		Priority:     SubnetEgressPriorityNATGW,
		ExcludeCIDRs: excludes,
	}))
	require.NoError(t, rm.DeleteSubnetEgress(ctx, "vpc-1", "subnet-pub", prefix, excludes))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies, "delete must match same predicate as add when excludes are present")
}
