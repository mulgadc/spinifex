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
		if p.ExternalIDs["spinifex:role"] == role {
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
		if got := peeringRows(m, router, "peering-route-drop"); got != 1 {
			t.Errorf("%s route drops = %d, want 1", router, got)
		}
		if got := peeringRows(m, router, "peering-reroute"); got != 1 {
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
		if got := peeringRows(m, router, "peering-reroute"); got != 1 {
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
	if got := peeringRows(m, "vpc-vpc-a", "peering-reroute"); got != 1 {
		t.Fatalf("reroutes = %d, want 1", got)
	}

	delete(m.RouterPorts, topology.PeeringRouterPort(testPcx, "vpc-b"))
	m.EnsurePeeringLinkErr = errors.New("nb unavailable")
	err := rec.Reconcile(ctx, peeringIntent(true))
	if !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, "peering-reroute"); got != 0 {
			t.Errorf("%s reroutes after failed link = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, "peering-route-drop"); got != 1 {
			t.Errorf("%s route drops after failed link = %d, want 1", router, got)
		}
	}
}

// One missing guard on an active link makes the next pass rewrite the whole
// link in one transaction; if that rewrite fails, the same pass withdraws the
// link's reroutes on both routers.
func TestReconcile_PeeringMissingGuardRewritesLink(t *testing.T) {
	rec, m := newTestReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	guards, err := policy.PeeringGuards(peeringSpec())
	if err != nil {
		t.Fatalf("PeeringGuards: %v", err)
	}
	dropGuard := func() {
		t.Helper()
		if err := m.DeleteLogicalRouterPolicy(ctx, "vpc-vpc-a", guards[0].Priority, guards[0].Match); err != nil {
			t.Fatalf("DeleteLogicalRouterPolicy: %v", err)
		}
	}

	dropGuard()
	writes := m.EnsurePeeringLinkCalls
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile repair: %v", err)
	}
	if m.EnsurePeeringLinkCalls != writes+1 {
		t.Errorf("link transactions = %d, want one", m.EnsurePeeringLinkCalls-writes)
	}
	if got := linkPortCount(m); got != 2 {
		t.Errorf("link ports = %d, want 2", got)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, ovn.PeeringGuardRole); got != 1 {
			t.Errorf("%s guards after repair = %d, want 1", router, got)
		}
		if got := peeringRows(m, router, "peering-reroute"); got != 1 {
			t.Errorf("%s reroutes after repair = %d, want 1", router, got)
		}
	}

	dropGuard()
	m.EnsurePeeringLinkErr = errors.New("nb unavailable")
	if err := rec.Reconcile(ctx, peeringIntent(true)); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		if got := peeringRows(m, router, "peering-reroute"); got != 0 {
			t.Errorf("%s reroutes with a guard missing = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, "peering-route-drop"); got != 1 {
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
		if got := peeringRows(m, router, "peering-reroute"); got != 0 {
			t.Errorf("%s reroutes after teardown = %d, want 0", router, got)
		}
		if got := peeringRows(m, router, "peering-route-drop"); got != 1 {
			t.Errorf("%s route drops after teardown = %d, want 1", router, got)
		}
	}
	if !slices.Equal(p.cleaned, []string{testPcx}) {
		t.Errorf("cleaned = %v, want [%s]", p.cleaned, testPcx)
	}
}

// A failed read of the peering bucket changes nothing: no link is torn down,
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
	if got := peeringRows(m, "vpc-vpc-a", "peering-reroute"); got != 1 {
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
		if got := peeringRows(m, router, "peering-reroute"); got != 0 {
			t.Errorf("%s reroutes while unconfirmed = %d, want 0", router, got)
		}
	}
}
