package reconcile

import (
	"context"
	"errors"
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

const testPcx = "pcx-0123456789abcdef0"

type peeringRecorder struct {
	barriers int
	active   map[string]int64
	cleaned  []string
	order    []string
}

func (p *peeringRecorder) wire(r *reconciler) {
	p.active = map[string]int64{}
	r.flowsBarrier = func() error {
		p.barriers++
		p.order = append(p.order, "barrier")
		return nil
	}
	r.markPeeringActive = func(_ context.Context, id string, gen int64) error {
		p.active[id] = gen
		p.order = append(p.order, "active:"+id)
		return nil
	}
	r.markPeeringCleaned = func(_ context.Context, id string) error {
		p.cleaned = append(p.cleaned, id)
		return nil
	}
}

func peeringSpec() topology.PeeringLinkSpec {
	return topology.PeeringLinkSpec{
		PcxID: testPcx, RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: netip.MustParsePrefix("10.0.0.0/16"), AccepterCIDR: netip.MustParsePrefix("10.1.0.0/16"),
		RequesterAddr: netip.MustParseAddr("169.254.192.1"), AccepterAddr: netip.MustParseAddr("169.254.192.2"),
	}
}

// peeringIntent is two local VPCs, one subnet each, each main table routing
// the other VPC's range over the connection.
func peeringIntent(active bool) IntentState {
	spec := peeringSpec()
	return IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: spec.RequesterCIDR, VNI: 100},
			"vpc-b": {VPCID: "vpc-b", CIDR: spec.AccepterCIDR, VNI: 101},
		},
		Subnets: map[string]topology.SubnetSpec{
			"subnet-a": {SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.1.0/24")},
			"subnet-b": {SubnetID: "subnet-b", VPCID: "vpc-b", CIDR: netip.MustParsePrefix("10.1.1.0/24")},
		},
		Ports:       map[string]topology.PortSpec{},
		SGs:         map[string]policy.SGSpec{},
		IGWs:        map[string]external.IGWSpec{},
		EIPs:        map[string]policy.EIPSpec{},
		NATGWs:      map[string]policy.NATGWSpec{},
		IGWRoutes:   map[string]SubnetEgressIntent{},
		NATGWRoutes: map[string]SubnetEgressIntent{},
		DropGates:   map[string]SubnetEgressIntent{},
		PeeringIntent: PeeringIntent{
			PeeringLinks:     map[string]PeeringLinkIntent{testPcx: {Spec: spec, Generation: 3, Active: active}},
			PeeringTeardowns: map[string]PeeringTeardownIntent{},
			PeeringLive:      map[string]struct{}{testPcx: {}},
			PeeringRoutes: []PeeringRouteIntent{
				{VPCID: "vpc-a", RouteTableID: "rtb-a", SubnetID: "subnet-a", Dest: spec.AccepterCIDR,
					Exclude: []netip.Prefix{spec.RequesterCIDR}, PcxID: testPcx},
				{VPCID: "vpc-b", RouteTableID: "rtb-b", SubnetID: "subnet-b", Dest: spec.RequesterCIDR,
					Exclude: []netip.Prefix{spec.AccepterCIDR}, PcxID: testPcx},
			},
		},
	}
}

// peeringRows counts the policies on router carrying role.
func peeringRows(m *mock.Client, router, role string) int {
	ctx := context.Background()
	rows, err := m.ListLogicalRouterPolicies(ctx, router)
	if err != nil {
		return -1
	}
	n := 0
	for _, p := range rows {
		if p.ExternalIDs[ovn.ExternalIDRole] == role {
			n++
		}
	}
	return n
}

func entryRouteCount(m *mock.Client, router string) int {
	routes, err := m.ListStaticRoutes(context.Background(), router)
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range routes {
		if r.ExternalIDs[ovn.ExternalIDPcxID] == testPcx {
			n++
		}
	}
	return n
}

func linkPortCount(m *mock.Client) int {
	n := 0
	for _, p := range m.RouterPorts {
		if p.ExternalIDs[ovn.ExternalIDPcxID] == testPcx {
			n++
		}
	}
	return n
}

// A provisioning connection gets its link and guards, is confirmed active only
// after the flows barrier, and its routes deliver in the pass that confirms it.
func TestReconcile_PeeringProvisioningThenActive(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()

	if err := rec.Reconcile(ctx, peeringIntent(false)); err != nil {
		t.Fatalf("Reconcile provisioning: %v", err)
	}
	if got := linkPortCount(m); got != 2 {
		t.Fatalf("link ports = %d, want 2", got)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, ovn.PeeringGuardRole); got != 1 {
			t.Errorf("%s guards = %d, want 1", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRouteDropRole); got != 1 {
			t.Errorf("%s route drops = %d, want 1", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 1 {
			t.Errorf("%s reroutes in the confirming pass = %d, want 1", router, got)
		}
	}
	if !slices.Equal(p.order, []string{"barrier", "active:" + testPcx}) {
		t.Errorf("activation order = %v, want barrier then active", p.order)
	}
	if p.active[testPcx] != 3 {
		t.Errorf("activated generation = %d, want 3", p.active[testPcx])
	}

	writes := m.EnsurePeeringLinkCalls
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile active: %v", err)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 1 {
			t.Errorf("%s reroutes when active = %d, want 1", router, got)
		}
	}
	if m.EnsurePeeringLinkCalls != writes {
		t.Errorf("steady-state pass rewrote the link: %d calls, want %d", m.EnsurePeeringLinkCalls, writes)
	}
	if p.barriers != 1 {
		t.Errorf("barriers = %d, want 1: an active link needs no confirmation", p.barriers)
	}
}

// A link whose transaction fails withdraws its reroutes on the same pass, so
// a route never forwards into a link whose guard may be missing.
func TestReconcile_PeeringFailedLinkWithdrawsReroute(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := peeringRows(m, "vpc-vpc-a", policy.PeeringRerouteRole); got != 1 {
		t.Fatalf("reroutes = %d, want 1", got)
	}

	delete(m.RouterPorts, topology.PeeringRouterPort(testPcx, "vpc-b"))
	m.EnsurePeeringLinkErr = errors.New("nb unavailable")
	err := rec.Reconcile(ctx, peeringIntent(true))
	if !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 0 {
			t.Errorf("%s reroutes after failed link = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRouteDropRole); got != 1 {
			t.Errorf("%s route drops after failed link = %d, want 1", router, got)
		}
	}
}

// A missing guard, entry route or no-route drop on an active link makes the
// next pass rewrite the whole link in one transaction, restoring both ends; a
// converged pass then writes nothing.
func TestReconcile_PeeringMissingRowRewritesLink(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	sides := peeringSpec().Sides()
	guard, noRoute := policy.PeeringGuard(sides[0]), policy.PeeringNoRouteDrop(sides[1])
	for _, tc := range []struct {
		name string
		drop func() error
	}{
		{"guard", func() error { return m.DeleteLogicalRouterPolicy(ctx, "vpc-vpc-a", guard.Priority, guard.Match) }},
		{"entry route", func() error { return m.DeleteStaticRoute(ctx, "vpc-vpc-a", sides[0].PeerCIDR.String()) }},
		{"no-route drop", func() error { return m.DeleteLogicalRouterPolicy(ctx, "vpc-vpc-b", noRoute.Priority, noRoute.Match) }},
	} {
		if err := tc.drop(); err != nil {
			t.Fatalf("delete %s: %v", tc.name, err)
		}
		writes := m.EnsurePeeringLinkCalls
		if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
			t.Fatalf("Reconcile after %s deleted: %v", tc.name, err)
		}
		if m.EnsurePeeringLinkCalls != writes+1 {
			t.Errorf("%s: link transactions = %d, want one", tc.name, m.EnsurePeeringLinkCalls-writes)
		}
		if got := linkPortCount(m); got != 2 {
			t.Errorf("%s: link ports = %d, want 2", tc.name, got)
		}
		for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
			for _, role := range []string{ovn.PeeringGuardRole, ovn.PeeringNoRouteRole, policy.PeeringRerouteRole} {
				if got := peeringRows(m, router, role); got != 1 {
					t.Errorf("%s: %s %s rows = %d, want 1", tc.name, router, role, got)
				}
			}
			if got := entryRouteCount(m, router); got != 1 {
				t.Errorf("%s: %s entry routes = %d, want 1", tc.name, router, got)
			}
		}
	}
	writes := m.EnsurePeeringLinkCalls
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile converged: %v", err)
	}
	if m.EnsurePeeringLinkCalls != writes {
		t.Errorf("a converged pass wrote the link %d times", m.EnsurePeeringLinkCalls-writes)
	}
}

// When rewriting a link with a missing guard fails, the same pass withdraws
// the link's reroutes on both routers and keeps the route drops.
func TestReconcile_PeeringFailedRewriteWithdrawsReroutes(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	guard := policy.PeeringGuard(peeringSpec().Sides()[0])
	if err := m.DeleteLogicalRouterPolicy(ctx, "vpc-vpc-a", guard.Priority, guard.Match); err != nil {
		t.Fatalf("DeleteLogicalRouterPolicy: %v", err)
	}
	m.EnsurePeeringLinkErr = errors.New("nb unavailable")
	if err := rec.Reconcile(ctx, peeringIntent(true)); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 0 {
			t.Errorf("%s reroutes with a guard missing = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRouteDropRole); got != 1 {
			t.Errorf("%s route drops with a guard missing = %d, want 1", router, got)
		}
	}
}

// A deleted connection loses its link, guards and reroutes and is reported
// cleaned; a route still naming it keeps its drop, so it blackholes.
func TestReconcile_PeeringTeardown(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	intent := peeringIntent(true)
	intent.PeeringLinks = map[string]PeeringLinkIntent{}
	intent.PeeringTeardowns = map[string]PeeringTeardownIntent{testPcx: {testPcx, "vpc-a", "vpc-b"}}
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile teardown: %v", err)
	}
	if got := linkPortCount(m); got != 0 {
		t.Errorf("link ports after teardown = %d, want 0", got)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, ovn.PeeringGuardRole); got != 0 {
			t.Errorf("%s guards after teardown = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, ovn.PeeringNoRouteRole); got != 0 {
			t.Errorf("%s no-route drops after teardown = %d, want 0", router, got)
		}
		if got := entryRouteCount(m, router); got != 0 {
			t.Errorf("%s entry routes after teardown = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 0 {
			t.Errorf("%s reroutes after teardown = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, policy.PeeringRouteDropRole); got != 1 {
			t.Errorf("%s route drops after teardown = %d, want 1", router, got)
		}
	}
	if !slices.Equal(p.cleaned, []string{testPcx}) {
		t.Errorf("cleaned = %v, want [%s]", p.cleaned, testPcx)
	}
}

// A failed read of the peering bucket changes nothing: no link is torn down,
// A teardown whose link delete fails is not reported cleaned, so the record
// and its link address are kept until a later pass removes the link.
func TestReconcile_PeeringFailedTeardownIsNotCleaned(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	intent := peeringIntent(true)
	intent.PeeringLinks = map[string]PeeringLinkIntent{}
	intent.PeeringTeardowns = map[string]PeeringTeardownIntent{testPcx: {testPcx, "vpc-a", "vpc-b"}}

	m.DeletePeeringLinkErr = errors.New("nb unavailable")
	if err := rec.Reconcile(ctx, intent); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile failed teardown = %v, want ErrPassIncomplete", err)
	}
	if len(p.cleaned) != 0 {
		t.Fatalf("cleaned = %v after a failed teardown, want none", p.cleaned)
	}
	if got := linkPortCount(m); got != 2 {
		t.Errorf("link ports after a failed teardown = %d, want 2", got)
	}

	m.DeletePeeringLinkErr = nil
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile retried teardown: %v", err)
	}
	if !slices.Equal(p.cleaned, []string{testPcx}) || linkPortCount(m) != 0 {
		t.Errorf("after retry cleaned = %v, link ports = %d; want [%s] and 0", p.cleaned, linkPortCount(m), testPcx)
	}
}

// no route is withdrawn, and the pass reports itself incomplete.
func TestReconcile_PeeringIncompleteIntentChangesNothing(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	intent := peeringIntent(true)
	intent.PeeringIntent = PeeringIntent{PeeringIncomplete: errors.New("bucket unreadable")}
	if err := rec.Reconcile(ctx, intent); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	if got := linkPortCount(m); got != 2 {
		t.Errorf("link ports = %d, want 2", got)
	}
	if got := peeringRows(m, "vpc-vpc-a", policy.PeeringRerouteRole); got != 1 {
		t.Errorf("reroutes = %d, want 1", got)
	}
}

// Rows stamped with a connection that has no live record are swept by a
// pruning pass and kept by an apply-only one.
func TestReconcile_PeeringOrphanPrunedOnlyWhenPruning(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	gone := peeringIntent(true)
	gone.PeeringLinks = map[string]PeeringLinkIntent{}
	gone.PeeringRoutes = nil
	gone.PeeringLive = map[string]struct{}{}
	if err := rec.ReconcileApplyOnly(ctx, gone); err != nil {
		t.Fatalf("ReconcileApplyOnly: %v", err)
	}
	if got := linkPortCount(m); got != 2 {
		t.Errorf("link ports after apply-only = %d, want 2", got)
	}
	if err := rec.Reconcile(ctx, gone); err != nil {
		t.Fatalf("Reconcile prune: %v", err)
	}
	if got := linkPortCount(m); got != 0 {
		t.Errorf("link ports after prune = %d, want 0", got)
	}
	if got := peeringRows(m, "vpc-vpc-a", ovn.PeeringGuardRole); got != 0 {
		t.Errorf("guards after prune = %d, want 0", got)
	}
}

// A connection whose activation report fails keeps only its route drops, so
// traffic never flows while the record still says provisioning.
func TestReconcile_PeeringUnconfirmedLinkDoesNotDeliver(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	rec.markPeeringActive = func(context.Context, string, int64) error { return errors.New("kv unavailable") }
	ctx := context.Background()

	if err := rec.Reconcile(ctx, peeringIntent(false)); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	if got := linkPortCount(m); got != 2 {
		t.Errorf("link ports = %d, want 2", got)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, policy.PeeringRerouteRole); got != 0 {
			t.Errorf("%s reroutes while unconfirmed = %d, want 0", router, got)
		}
	}
}

type failingPeeringRoutes struct{ err error }

func (f failingPeeringRoutes) SyncPeeringRoutes(context.Context, string, []nbdb.LogicalRouterPolicy) error {
	return f.err
}

// A router deleted after the pass snapshot is skipped without failing the
// pass; the surviving VPC still syncs.
func TestApplyPeeringRoutes_RouterGoneMidPassIsSkipped(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-b")}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	actual := ActualState{Routers: map[string]struct{}{
		topology.VPCRouter("vpc-a"): {}, topology.VPCRouter("vpc-b"): {},
	}}
	res := &passResult{}
	rec.applyPeeringRoutes(ctx, peeringIntent(true), actual, nil, res)
	if len(res.failures) != 0 {
		t.Fatalf("failures = %v, want none", res.failures)
	}
	if got := peeringRows(m, topology.VPCRouter("vpc-b"), policy.PeeringRouteDropRole); got != 1 {
		t.Fatalf("vpc-b drop rows = %d, want 1", got)
	}
}

func TestApplyPeeringRoutes_OtherSyncErrorFailsPass(t *testing.T) {
	rec, _ := newTestReconciler(t)
	rec.peeringRoutes = failingPeeringRoutes{err: errors.New("ovsdb transact failed")}
	actual := ActualState{Routers: map[string]struct{}{topology.VPCRouter("vpc-a"): {}}}
	res := &passResult{}
	rec.applyPeeringRoutes(context.Background(), peeringIntent(true), actual, nil, res)
	if len(res.failures) != 1 || res.failures[0].class != classPeeringRoute || res.failures[0].id != "vpc-a" {
		t.Fatalf("failures = %v, want one peering_route failure for vpc-a", res.failures)
	}
}
