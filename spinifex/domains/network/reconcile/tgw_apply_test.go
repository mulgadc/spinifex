package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

const (
	testTGW  = "tgw-1"
	testAttA = "tgw-attach-a"
	testAttB = "tgw-attach-b"
	testAcct = "111122223333"
)

// tgwRecorder captures every hook call in order.
type tgwRecorder struct {
	calls      []string
	failActive error
}

func (p *tgwRecorder) wire(r *reconciler) {
	r.flowsBarrier = func() error { p.calls = append(p.calls, "barrier"); return nil }
	r.tgwHooks = TransitGatewayHooks{
		PrepareAttachment: func(_ context.Context, acct, id string) error {
			p.calls = append(p.calls, "prepare:"+acct+":"+id)
			return nil
		},
		MarkTransitGatewayAvailable: func(_ context.Context, acct, id string, gen int64) error {
			p.calls = append(p.calls, fmt.Sprintf("tgw-available:%s:%s:%d", acct, id, gen))
			return nil
		},
		MarkTransitGatewayCleaned: func(_ context.Context, acct, id string) error {
			p.calls = append(p.calls, "tgw-cleaned:"+acct+":"+id)
			return nil
		},
		MarkAttachmentAvailable: func(_ context.Context, _, id string, gen int64) error {
			if p.failActive != nil {
				return p.failActive
			}
			p.calls = append(p.calls, fmt.Sprintf("available:%s:%d", id, gen))
			return nil
		},
		MarkAttachmentCleaned: func(_ context.Context, _, id string) error {
			p.calls = append(p.calls, "cleaned:"+id)
			return nil
		},
	}
}

var (
	cidrA = netip.MustParsePrefix("10.0.0.0/16")
	cidrB = netip.MustParsePrefix("10.1.0.0/16")
)

func tgwLink(id, vpcID string, cidr netip.Prefix, slot int, available bool, azs ...string) TGWLinkIntent {
	zones := map[string]struct{}{}
	for _, az := range azs {
		zones[az] = struct{}{}
	}
	base := netip.MustParseAddr("169.254.64.0").As4()
	base[3] = byte(slot * 4)
	vpcAddr := netip.AddrFrom4(base).Next()
	return TGWLinkIntent{
		Spec: topology.TransitGatewayLinkSpec{
			AttachmentID: id, TransitGatewayID: testTGW, VPCID: vpcID, VPCCIDR: cidr,
			VPCAddr: vpcAddr, TGWAddr: vpcAddr.Next(),
		},
		AccountID: testAcct, Generation: 4, Available: available, Holds: true, AZs: zones,
	}
}

// tgwIntent is two VPCs attached to one Transit Gateway with routes to each
// other. VPC A has a subnet in us-east-1a, where its attachment has a subnet,
// and one in us-east-1b, where it has none.
func tgwIntent(available bool) IntentState {
	intent := peeringIntent(true)
	intent.PeeringIntent = PeeringIntent{
		PeeringLinks: map[string]PeeringLinkIntent{}, PeeringTeardowns: map[string]PeeringTeardownIntent{},
		PeeringLive: map[string]struct{}{},
	}
	intent.Subnets["subnet-a2"] = topology.SubnetSpec{SubnetID: "subnet-a2", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.2.0/24")}
	ti := newTransitGatewayIntent(map[string]string{"subnet-a": "us-east-1a", "subnet-a2": "us-east-1b", "subnet-b": "us-east-1a"})
	ti.TGWRouters[testTGW] = TGWRouterIntent{AccountID: testAcct, Generation: 1}
	ti.TGWLinks[testAttA] = tgwLink(testAttA, "vpc-a", cidrA, 1, available, "us-east-1a")
	ti.TGWLinks[testAttB] = tgwLink(testAttB, "vpc-b", cidrB, 2, available, "us-east-1a")
	for _, id := range []string{testTGW, testAttA, testAttB} {
		ti.TGWLive[id] = struct{}{}
	}
	ti.TGWRoutes = []TGWRouteIntent{
		{VPCID: "vpc-a", RouteTableID: "rtb-a", SubnetID: "subnet-a", Dest: cidrB, Exclude: []netip.Prefix{cidrA}, TransitGatewayID: testTGW},
		{VPCID: "vpc-a", RouteTableID: "rtb-a", SubnetID: "subnet-a2", Dest: cidrB, Exclude: []netip.Prefix{cidrA}, TransitGatewayID: testTGW},
		{VPCID: "vpc-b", RouteTableID: "rtb-b", SubnetID: "subnet-b", Dest: cidrA, Exclude: []netip.Prefix{cidrB}, TransitGatewayID: testTGW},
	}
	intent.TransitGatewayIntent = ti
	return intent
}

func policiesWith(t *testing.T, m *mock.Client, router, role string) []nbdb.LogicalRouterPolicy {
	t.Helper()
	rows, err := m.ListLogicalRouterPolicies(context.Background(), router)
	if err != nil {
		t.Fatalf("ListLogicalRouterPolicies %s: %v", router, err)
	}
	return slices.DeleteFunc(rows, func(p nbdb.LogicalRouterPolicy) bool { return p.ExternalIDs[ovn.ExternalIDRole] != role })
}

func routesWith(t *testing.T, m *mock.Client, router, role string) []string {
	t.Helper()
	rows, err := m.ListStaticRoutes(context.Background(), router)
	if err != nil {
		t.Fatalf("ListStaticRoutes %s: %v", router, err)
	}
	var out []string
	for _, r := range rows {
		if r.ExternalIDs[ovn.ExternalIDRole] == role {
			out = append(out, r.IPPrefix+" via "+r.Nexthop)
		}
	}
	slices.Sort(out)
	return out
}

func reroutedSubnets(t *testing.T, m *mock.Client, router string) []string {
	t.Helper()
	var out []string
	for _, p := range policiesWith(t, m, router, policy.TransitGatewayRerouteRole) {
		out = append(out, p.ExternalIDs["spinifex:subnet"])
	}
	slices.Sort(out)
	return out
}

// A pending Transit Gateway gets its router and is then confirmed at its
// generation; an available one is not confirmed again.
func TestReconcile_TransitGatewayRouterThenAvailable(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	intent := tgwIntent(true)
	intent.TGWRouters[testTGW] = TGWRouterIntent{AccountID: testAcct, Generation: 2, Pending: true}
	if err := rec.Reconcile(context.Background(), intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	lr, ok := m.Routers[topology.TransitGatewayRouter(testTGW)]
	if !ok || lr.ExternalIDs[ovn.ExternalIDTGWID] != testTGW {
		t.Fatalf("Transit Gateway router = %+v, want one stamped %s", lr, testTGW)
	}
	if !slices.Contains(p.calls, "tgw-available:"+testAcct+":"+testTGW+":2") {
		t.Errorf("calls = %v, want the router confirmed at generation 2", p.calls)
	}

	p.calls = nil
	intent.TGWRouters[testTGW] = TGWRouterIntent{AccountID: testAcct, Generation: 2}
	if err := rec.Reconcile(context.Background(), intent); err != nil {
		t.Fatalf("Reconcile available: %v", err)
	}
	if slices.ContainsFunc(p.calls, func(c string) bool { return c[:4] == "tgw-" }) {
		t.Errorf("calls = %v, want no Transit Gateway report when available", p.calls)
	}
}

// A pending attachment gets both ends of its link, is confirmed only after
// the flows barrier, and delivers in the pass that confirms it. A steady
// pass writes nothing.
func TestReconcile_TransitGatewayLinkPendingThenAvailable(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, tgwIntent(false)); err != nil {
		t.Fatalf("Reconcile pending: %v", err)
	}
	vpcPort, ok := m.RouterPorts[topology.TransitGatewayVPCPort(testAttA)]
	if !ok || vpcPort.Peer == nil || *vpcPort.Peer != topology.TransitGatewayPort(testAttA) || vpcPort.Networks[0] != "169.254.64.5/30" {
		t.Fatalf("VPC link port = %+v", vpcPort)
	}
	tgwPort, ok := m.RouterPorts[topology.TransitGatewayPort(testAttA)]
	if !ok || !slices.Contains(m.Routers[topology.TransitGatewayRouter(testTGW)].Ports, tgwPort.UUID) {
		t.Fatal("Transit Gateway link port is not on the Transit Gateway router")
	}
	if got := routesWith(t, m, topology.TransitGatewayRouter(testTGW), policy.TransitGatewayPropagationRole); !slices.Equal(got, []string{"10.0.0.0/16 via 169.254.64.5", "10.1.0.0/16 via 169.254.64.9"}) {
		t.Errorf("propagated routes = %v", got)
	}
	if got := routesWith(t, m, "vpc-vpc-a", policy.TransitGatewayEntryRole); !slices.Equal(got, []string{"10.1.0.0/16 via 169.254.64.6"}) {
		t.Errorf("VPC A entry routes = %v", got)
	}
	for _, role := range []string{policy.TransitGatewayGuardRole, policy.TransitGatewayNoRouteRole} {
		if got := len(policiesWith(t, m, "vpc-vpc-a", role)); got != 1 {
			t.Errorf("VPC A %s rows = %d, want 1", role, got)
		}
	}
	want := []string{"barrier", "available:" + testAttA + ":4", "available:" + testAttB + ":4"}
	if !slices.Equal(p.calls, want) {
		t.Errorf("calls = %v, want %v", p.calls, want)
	}
	if got := reroutedSubnets(t, m, "vpc-vpc-a"); !slices.Equal(got, []string{"subnet-a"}) {
		t.Errorf("VPC A reroutes in the confirming pass = %v, want subnet-a", got)
	}

	writes := m.EnsureTransitGatewayLinkWrites
	p.calls = nil
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile available: %v", err)
	}
	if m.EnsureTransitGatewayLinkWrites != writes {
		t.Errorf("steady pass changed a link: %d writes, want %d", m.EnsureTransitGatewayLinkWrites, writes)
	}
	if len(p.calls) != 0 {
		t.Errorf("calls = %v, want none: an available link needs no confirmation", p.calls)
	}
}

// A source subnet in a zone with no attachment subnet keeps only its drop.
func TestReconcile_TransitGatewayRouteNeedsAttachmentZone(t *testing.T) {
	rec, m := newTestReconciler(t)
	if err := rec.Reconcile(context.Background(), tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	drops := policiesWith(t, m, "vpc-vpc-a", policy.TransitGatewayRouteDropRole)
	if len(drops) != 2 {
		t.Fatalf("VPC A route drops = %d, want one per governed subnet", len(drops))
	}
	reroutes := policiesWith(t, m, "vpc-vpc-a", policy.TransitGatewayRerouteRole)
	if len(reroutes) != 1 || reroutes[0].ExternalIDs["spinifex:subnet"] != "subnet-a" {
		t.Fatalf("VPC A reroutes = %+v, want subnet-a only", reroutes)
	}
	r := reroutes[0]
	if !slices.Equal(r.Nexthops, []string{"169.254.64.6"}) || r.ExternalIDs[ovn.ExternalIDTGWAttachmentID] != testAttA ||
		r.Match != `inport == "rtr-subnet-a" && ip4.dst == 10.1.0.0/16` {
		t.Errorf("reroute = %+v", r)
	}
	if r.Priority != drops[0].Priority+1 {
		t.Errorf("reroute priority %d, want one above its drop %d", r.Priority, drops[0].Priority)
	}
}

// An attachment whose confirmation fails delivers nothing: its routes keep
// only their drops, and the pass reports the link.
func TestReconcile_TransitGatewayUnconfirmedLinkDoesNotDeliver(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{failActive: errors.New("kv down")}
	p.wire(rec)
	err := rec.Reconcile(context.Background(), tgwIntent(false))
	if !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	if got := reroutedSubnets(t, m, "vpc-vpc-a"); len(got) != 0 {
		t.Errorf("reroutes = %v, want none before confirmation", got)
	}
	if got := len(policiesWith(t, m, "vpc-vpc-a", policy.TransitGatewayRouteDropRole)); got != 2 {
		t.Errorf("route drops = %d, want 2", got)
	}
}

// Deleting an attachment removes every row it owns on both routers and
// reports it cleaned; the route that targeted it keeps its drop, so it is a
// blackhole and not a fall-through to a less specific route.
func TestReconcile_TransitGatewayTeardownLeavesBlackhole(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	intent := tgwIntent(true)
	delete(intent.TGWLinks, testAttB)
	intent.TGWLinkTeardowns[testAttB] = TGWLinkTeardownIntent{testAcct, testTGW, "vpc-b"}
	p.calls = nil
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile teardown: %v", err)
	}
	if !slices.Equal(p.calls, []string{"cleaned:" + testAttB}) {
		t.Errorf("calls = %v, want cleaned", p.calls)
	}
	for name, port := range m.RouterPorts {
		if port.ExternalIDs[ovn.ExternalIDTGWAttachmentID] == testAttB {
			t.Errorf("port %s of the deleted attachment remains", name)
		}
	}
	if got := routesWith(t, m, topology.TransitGatewayRouter(testTGW), policy.TransitGatewayPropagationRole); !slices.Equal(got, []string{"10.0.0.0/16 via 169.254.64.5"}) {
		t.Errorf("propagated routes = %v, want only VPC A's", got)
	}
	for _, role := range []string{policy.TransitGatewayGuardRole, policy.TransitGatewayNoRouteRole, policy.TransitGatewayRerouteRole} {
		if got := len(policiesWith(t, m, "vpc-vpc-b", role)); got != 0 {
			t.Errorf("VPC B %s rows = %d, want 0", role, got)
		}
	}
	if got := len(policiesWith(t, m, "vpc-vpc-b", policy.TransitGatewayRouteDropRole)); got != 1 {
		t.Errorf("VPC B route drops = %d, want 1: the route is a blackhole", got)
	}
}

// A summary route gets one entry route; a default route gets one only when
// no internet gateway's default route already carries it.
func TestReconcile_TransitGatewayEntryRoutes(t *testing.T) {
	summary := netip.MustParsePrefix("10.0.0.0/8")
	for _, tc := range []struct {
		name string
		dest netip.Prefix
		igw  bool
		want []string
	}{
		{"summary", summary, false, []string{"10.0.0.0/8 via 169.254.64.10"}},
		{"default without gateway", defaultRoute, false, []string{"0.0.0.0/0 via 169.254.64.10"}},
		{"default with gateway", defaultRoute, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, m := newTestReconciler(t)
			intent := tgwIntent(true)
			intent.TGWRoutes[2].Dest = tc.dest
			if tc.igw {
				intent.IGWs["igw-b"] = external.IGWSpec{VPCID: "vpc-b", InternetGatewayID: "igw-b"}
			}
			_ = rec.Reconcile(context.Background(), intent)
			if got := routesWith(t, m, "vpc-vpc-b", policy.TransitGatewayEntryRole); !slices.Equal(got, tc.want) {
				t.Errorf("VPC B entry routes = %v, want %v", got, tc.want)
			}
			if got := reroutedSubnets(t, m, "vpc-vpc-b"); !slices.Equal(got, []string{"subnet-b"}) {
				t.Errorf("VPC B reroutes = %v, want subnet-b", got)
			}
		})
	}
}

// A deleting Transit Gateway loses its router and is reported cleaned.
func TestReconcile_TransitGatewayTeardownDeletesRouter(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	ctx := context.Background()
	intent := tgwIntent(true)
	intent.TGWLinks = map[string]TGWLinkIntent{}
	intent.TGWRoutes = nil
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	delete(intent.TGWRouters, testTGW)
	intent.TGWTeardowns[testTGW] = TGWOwner{testAcct, testTGW}
	p.calls = nil
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile teardown: %v", err)
	}
	if _, ok := m.Routers[topology.TransitGatewayRouter(testTGW)]; ok {
		t.Error("router of the deleting Transit Gateway remains")
	}
	if !slices.Equal(p.calls, []string{"tgw-cleaned:" + testAcct + ":" + testTGW}) {
		t.Errorf("calls = %v, want cleaned", p.calls)
	}
}

// A pending attachment with no link is prepared and not realised.
func TestReconcile_TransitGatewayPrepareBeforeLink(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	intent := tgwIntent(true)
	delete(intent.TGWLinks, testAttB)
	intent.TGWPrepare[testAttB] = TGWOwner{testAcct, testAttB}
	if err := rec.Reconcile(context.Background(), intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(p.calls, []string{"prepare:" + testAcct + ":" + testAttB}) {
		t.Errorf("calls = %v, want one prepare", p.calls)
	}
	if _, ok := m.RouterPorts[topology.TransitGatewayVPCPort(testAttB)]; ok {
		t.Error("an unprepared attachment got a link")
	}
}

// The prune removes rows and routers whose record is gone and keeps live ones.
func TestReconcile_TransitGatewayPruneOrphans(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	stale := "tgw-stale"
	if _, _, err := m.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: topology.TransitGatewayRouter(stale), ExternalIDs: map[string]string{ovn.ExternalIDTGWID: stale},
	}); err != nil {
		t.Fatalf("EnsureLogicalRouter: %v", err)
	}
	intent := tgwIntent(true)
	delete(intent.TGWLinks, testAttB)
	delete(intent.TGWLive, testAttB)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := m.Routers[topology.TransitGatewayRouter(stale)]; ok {
		t.Error("orphan Transit Gateway router remains")
	}
	if _, ok := m.Routers[topology.TransitGatewayRouter(testTGW)]; !ok {
		t.Error("live Transit Gateway router was pruned")
	}
	if _, ok := m.RouterPorts[topology.TransitGatewayVPCPort(testAttB)]; ok {
		t.Error("orphan attachment port remains")
	}
	if _, ok := m.RouterPorts[topology.TransitGatewayVPCPort(testAttA)]; !ok {
		t.Error("live attachment port was pruned")
	}
}

// An unreadable Transit Gateway bucket changes nothing and fails the pass.
func TestReconcile_TransitGatewayIncompleteChangesNothing(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	before := len(m.RouterPorts)
	drops := len(policiesWith(t, m, "vpc-vpc-a", policy.TransitGatewayRerouteRole))
	intent := tgwIntent(true)
	intent.TransitGatewayIntent = newTransitGatewayIntent(nil)
	intent.TGWIncomplete = errors.New("bucket unreadable")
	if err := rec.Reconcile(ctx, intent); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	if len(m.RouterPorts) != before || len(policiesWith(t, m, "vpc-vpc-a", policy.TransitGatewayRerouteRole)) != drops {
		t.Error("an incomplete read changed Transit Gateway rows")
	}
	if _, ok := m.Routers[topology.TransitGatewayRouter(testTGW)]; !ok {
		t.Error("an incomplete read removed the router")
	}
}
