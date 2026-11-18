package policy

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

func testLinkSpec() topology.PeeringLinkSpec {
	return topology.PeeringLinkSpec{
		PcxID: "pcx-1", RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: netip.MustParsePrefix("10.0.0.0/16"), AccepterCIDR: netip.MustParsePrefix("10.1.0.0/16"),
		RequesterAddr: netip.MustParseAddr("169.254.192.1"), AccepterAddr: netip.MustParseAddr("169.254.192.2"),
	}
}

// Each guard admits only peer-to-local IPv4 arriving on its own link port.
func TestPeeringGuard_Match(t *testing.T) {
	want := [2]string{
		`inport == "pcx-pcx-1-vpc-a" && (ip4.src != 10.1.0.0/16 || ip4.dst != 10.0.0.0/16 || ip6)`,
		`inport == "pcx-pcx-1-vpc-b" && (ip4.src != 10.0.0.0/16 || ip4.dst != 10.1.0.0/16 || ip6)`,
	}
	for i, side := range testLinkSpec().Sides() {
		if g := PeeringGuard(side); g.Match != want[i] || g.Action != "drop" || g.Priority != PeeringGuardPriority {
			t.Errorf("guard %d = %+v, want drop at %d on %s", i, g, PeeringGuardPriority, want[i])
		}
	}
}

// Each end's entry route leads to the peer over its own port, and its drop
// ranks above the system-instance allow and below every peering route.
func TestPeeringEntryRouteAndNoRouteDrop(t *testing.T) {
	want := [2]struct{ prefix, nexthop, port string }{
		{"10.1.0.0/16", "169.254.192.2", "pcx-pcx-1-vpc-a"},
		{"10.0.0.0/16", "169.254.192.1", "pcx-pcx-1-vpc-b"},
	}
	for i, side := range testLinkSpec().Sides() {
		r := PeeringEntryRoute(side)
		if r.IPPrefix != want[i].prefix || r.Nexthop != want[i].nexthop || r.OutputPort == nil || *r.OutputPort != want[i].port {
			t.Errorf("entry route %d = %+v, want %+v", i, r, want[i])
		}
		if d := PeeringNoRouteDrop(side); d.Match != "ip4.dst == "+want[i].prefix || d.Action != "drop" || d.Priority != PeeringNoRoutePriority {
			t.Errorf("no-route drop %d = %+v", i, d)
		}
	}
	if SystemInstanceEgressPriority >= PeeringNoRoutePriority || PeeringNoRoutePriority >= peeringRouteBase {
		t.Errorf("no-route drop at %d is not between the system-instance allow (%d) and the peering routes (%d)",
			PeeringNoRoutePriority, SystemInstanceEgressPriority, peeringRouteBase)
	}
}

func reroute() *PeeringReroute {
	return &PeeringReroute{
		PcxID: "pcx-1", Nexthop: netip.MustParseAddr("169.254.192.2"),
		OutputPort: "pcx-pcx-1-vpc-a", PeerCIDR: netip.MustParsePrefix("10.1.0.0/16"),
	}
}

// A route wider than the peer delivers only the peer's range, ranked by the
// route's own length, and drops the rest of its destination.
func TestPeeringRouteRows_WideRoute(t *testing.T) {
	rows := PeeringRouteRows(PeeringRouteSpec{
		RouteTableID: "rtb-1", SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.0.0.0/8"),
		Exclude: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("192.168.0.0/16")},
		Reroute: reroute(),
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	drop, rr := rows[0], rows[1]
	if drop.Priority != 1316 || rr.Priority != 1317 {
		t.Errorf("priorities = %d, %d, want 1316, 1317", drop.Priority, rr.Priority)
	}
	if drop.Match != `inport == "rtr-subnet-1" && ip4.dst == 10.0.0.0/8 && ip4.dst != 10.0.0.0/16` {
		t.Errorf("drop match = %s", drop.Match)
	}
	if rr.Match != `inport == "rtr-subnet-1" && ip4.dst == 10.1.0.0/16` {
		t.Errorf("reroute match = %s", rr.Match)
	}
	if rr.Action != "reroute" || rr.ExternalIDs[ovn.ExternalIDPcxID] != "pcx-1" {
		t.Errorf("reroute = %+v", rr)
	}
	if rr.Nexthop != nil || len(rr.Nexthops) != 1 || rr.Nexthops[0] != "169.254.192.2" {
		t.Errorf("reroute next hop = %v / %v, want only nexthops [169.254.192.2]", rr.Nexthop, rr.Nexthops)
	}
	if _, ok := drop.ExternalIDs[ovn.ExternalIDPcxID]; ok {
		t.Error("route drop carries the connection ID, so teardown would remove it")
	}
}

// A route narrower than the peer delivers exactly its own destination.
func TestPeeringRouteRows_NarrowRoute(t *testing.T) {
	rows := PeeringRouteRows(PeeringRouteSpec{
		RouteTableID: "rtb-1", SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.1.2.0/24"), Reroute: reroute(),
	})
	if len(rows) != 2 || rows[1].Match != `inport == "rtr-subnet-1" && ip4.dst == 10.1.2.0/24` {
		t.Fatalf("rows = %+v", rows)
	}
}

// A route outside the peer's range, or over an inactive link, only drops.
func TestPeeringRouteRows_DropOnly(t *testing.T) {
	outside := PeeringRouteRows(PeeringRouteSpec{SubnetID: "subnet-1", Dest: netip.MustParsePrefix("172.16.0.0/12"), Reroute: reroute()})
	inactive := PeeringRouteRows(PeeringRouteSpec{SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.1.0.0/16")})
	if len(outside) != 1 || len(inactive) != 1 || outside[0].Action != "drop" || inactive[0].Action != "drop" {
		t.Errorf("outside = %+v, inactive = %+v", outside, inactive)
	}
}

// Sync writes wanted rows, rewrites a drifted one, removes stale peering
// route rows, and leaves guards and other policies alone.
func TestSyncPeeringRoutes(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-vpc-a"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	guard := PeeringGuard(testLinkSpec().Sides()[0])
	other := nbdb.LogicalRouterPolicy{Priority: 1100, Match: "inport == \"rtr-subnet-1\"", Action: "drop"}
	stale := PeeringRouteRows(PeeringRouteSpec{SubnetID: "subnet-9", Dest: netip.MustParsePrefix("10.9.0.0/16")})[0]
	for _, p := range []nbdb.LogicalRouterPolicy{guard, other, stale} {
		if err := m.AddLogicalRouterPolicy(ctx, "vpc-vpc-a", &p); err != nil {
			t.Fatalf("AddLogicalRouterPolicy: %v", err)
		}
	}
	mgr := NewPeeringRouteManager(m)
	spec := PeeringRouteSpec{RouteTableID: "rtb-1", SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.1.0.0/16"), Reroute: reroute()}
	if err := mgr.SyncPeeringRoutes(ctx, "vpc-a", PeeringRouteRows(spec)); err != nil {
		t.Fatalf("SyncPeeringRoutes: %v", err)
	}
	spec.Reroute.Nexthop = netip.MustParseAddr("169.254.192.6")
	want := PeeringRouteRows(spec)
	if err := mgr.SyncPeeringRoutes(ctx, "vpc-a", want); err != nil {
		t.Fatalf("SyncPeeringRoutes drift: %v", err)
	}
	for _, p := range []nbdb.LogicalRouterPolicy{guard, other, want[0], want[1]} {
		got, err := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", p.Priority, p.Match)
		if err != nil || got == nil {
			t.Errorf("policy %q missing: %v", p.Match, err)
		}
	}
	if got, _ := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", want[1].Priority, want[1].Match); got != nil && !slices.Equal(got.Nexthops, []string{"169.254.192.6"}) {
		t.Errorf("drifted nexthops = %v, want [169.254.192.6]", got.Nexthops)
	}
	if got, _ := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", stale.Priority, stale.Match); got != nil {
		t.Error("stale peering route row survived")
	}
}

// A subnet moving to another table with the same blackhole destination keeps
// its drop throughout: the row is rewritten in place, and a failed rewrite
// leaves the old drop rather than none.
func TestSyncPeeringRoutes_ReassociationKeepsDrop(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-vpc-a"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	mgr := NewPeeringRouteManager(m)
	spec := PeeringRouteSpec{RouteTableID: "rtb-1", SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.1.0.0/16")}
	if err := mgr.SyncPeeringRoutes(ctx, "vpc-a", PeeringRouteRows(spec)); err != nil {
		t.Fatalf("SyncPeeringRoutes: %v", err)
	}
	drop := PeeringRouteRows(spec)[0]
	before, _ := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", drop.Priority, drop.Match)
	if before == nil {
		t.Fatal("drop not written")
	}

	spec.RouteTableID = "rtb-2"
	m.UpdateLogicalRouterPolicyErr = errInjected
	if err := mgr.SyncPeeringRoutes(ctx, "vpc-a", PeeringRouteRows(spec)); err == nil {
		t.Fatal("SyncPeeringRoutes succeeded with the rewrite failing")
	}
	if got, _ := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", drop.Priority, drop.Match); got == nil || got.Action != "drop" {
		t.Fatalf("drop after a failed rewrite = %+v, want the old drop", got)
	}

	m.UpdateLogicalRouterPolicyErr = nil
	if err := mgr.SyncPeeringRoutes(ctx, "vpc-a", PeeringRouteRows(spec)); err != nil {
		t.Fatalf("SyncPeeringRoutes: %v", err)
	}
	got, _ := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", drop.Priority, drop.Match)
	if got == nil || got.UUID != before.UUID || got.ExternalIDs["spinifex:rtb_id"] != "rtb-2" {
		t.Errorf("drop = %+v, want row %s rewritten to rtb-2", got, before.UUID)
	}
}

var errInjected = errors.New("injected")

// A reroute written through the deprecated nexthop column, which OVN 26.03
// ignores, is rewritten in place to use nexthops.
func TestSyncPeeringRoutes_RewritesDeprecatedNexthop(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-vpc-a"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	want := PeeringRouteRows(PeeringRouteSpec{RouteTableID: "rtb-1", SubnetID: "subnet-1", Dest: netip.MustParsePrefix("10.1.0.0/16"), Reroute: reroute()})
	legacy := want[1]
	nexthop := legacy.Nexthops[0]
	legacy.Nexthop, legacy.Nexthops = &nexthop, nil
	for _, p := range []nbdb.LogicalRouterPolicy{want[0], legacy} {
		if err := m.AddLogicalRouterPolicy(ctx, "vpc-vpc-a", &p); err != nil {
			t.Fatalf("AddLogicalRouterPolicy: %v", err)
		}
	}
	if err := NewPeeringRouteManager(m).SyncPeeringRoutes(ctx, "vpc-a", want); err != nil {
		t.Fatalf("SyncPeeringRoutes: %v", err)
	}
	got, err := m.FindLogicalRouterPolicy(ctx, "vpc-vpc-a", want[1].Priority, want[1].Match)
	if err != nil || got == nil {
		t.Fatalf("reroute missing: %v", err)
	}
	if got.Nexthop != nil || !slices.Equal(got.Nexthops, []string{"169.254.192.2"}) {
		t.Errorf("reroute next hop = %v / %v, want only nexthops [169.254.192.2]", got.Nexthop, got.Nexthops)
	}
}
