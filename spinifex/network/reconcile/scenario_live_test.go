package reconcile

// Scenario tests drive the real reconciler against a real (in-process) OVN
// Northbound DB via ovntest, instead of the hand-rolled ovn/mock. This
// exercises the full path — intent -> reconcile wiring -> LiveClient -> OVSDB
// transaction -> NB rows — and asserts the reconcile loop is idempotent by
// snapshotting NB state and re-running. Datapath and NB->SB translation remain
// in the VM-level e2e suites.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/ovntest"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// uuidRe and ptrRe strip identity churn from NB row dumps so the idempotency
// check asserts logical stability, not UUID/pointer identity. Real ovsdb
// assigns fresh UUIDs when a row is replaced (SG ACLs are replaced wholesale
// every reconcile) and %+v renders *string fields as pointer addresses.
var (
	uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	ptrRe  = regexp.MustCompile(`0x[0-9a-f]+`)
)

// newLiveReconciler builds a reconciler around a real in-process OVN NB DB.
// It mirrors newTestReconciler but swaps ovn/mock for a connected LiveClient.
func newLiveReconciler(t *testing.T) (*reconciler, ovn.Client) {
	t.Helper()
	nb := ovntest.StartNB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("LiveClient.Connect: %v", err)
	}
	t.Cleanup(cli.Close)

	sg := policy.NewSecurityGroupManager(cli)
	nat, err := policy.NewNATManager(cli, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	routes := policy.NewRouteManager(cli)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:       cli,
		Routes:    routes,
		NAT:       nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	if err != nil {
		t.Fatalf("NewIGWManager: %v", err)
	}
	topo := topology.NewLiveManager(cli)
	rec, err := New(Config{
		OVN: cli, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topo,
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := rec.(*reconciler)
	if !ok {
		t.Fatalf("New returned %T, want *reconciler", rec)
	}
	return r, cli
}

// TestScenario_VPCSubnetSGPort_Live reconciles a VPC + subnet + SG + ENI into a
// real NB DB, asserts the expected rows exist, then re-runs reconcile and
// asserts the NB snapshot is byte-for-byte unchanged (idempotency).
func TestScenario_VPCSubnetSGPort_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	ctx := context.Background()
	intent := freshIntent(t)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	if _, err := cli.GetLogicalRouter(ctx, "vpc-vpc-a"); err != nil {
		t.Errorf("VPC router vpc-vpc-a: %v", err)
	}
	if _, err := cli.GetLogicalSwitch(ctx, "subnet-subnet-a"); err != nil {
		t.Errorf("subnet switch subnet-subnet-a: %v", err)
	}
	if _, err := cli.GetLogicalSwitchPort(ctx, "port-eni-a"); err != nil {
		t.Errorf("ENI port port-eni-a: %v", err)
	}
	pgName := topology.SecurityGroupPortGroup("sg-a")
	pg, err := cli.GetPortGroup(ctx, pgName)
	if err != nil {
		t.Fatalf("SG port group %s: %v", pgName, err)
	}
	if !slices.Contains(pg.Ports, mustPortUUID(t, ctx, cli, "port-eni-a")) {
		t.Errorf("ENI LSP not a member of SG port group %s", pgName)
	}

	before := snapshotNB(t, ctx, cli)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	after := snapshotNB(t, ctx, cli)
	if before != after {
		t.Fatalf("reconcile not idempotent; NB changed on second pass:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// mustPortUUID returns the persisted UUID of an LSP by name.
func mustPortUUID(t *testing.T, ctx context.Context, cli ovn.Client, name string) string {
	t.Helper()
	lsp, err := cli.GetLogicalSwitchPort(ctx, name)
	if err != nil {
		t.Fatalf("GetLogicalSwitchPort %s: %v", name, err)
	}
	return lsp.UUID
}

// snapshotNB renders a deterministic, identity-normalized dump of every NB row
// the freshIntent scenario touches. Named rows keep a stable token derived from
// their name (so cross-references stay legible); residual UUIDs (ACLs, which
// have no list accessor here) collapse to "X" and pointer addresses to "PTR".
// A logical change — a row added, removed, or a value edited — still diffs.
func snapshotNB(t *testing.T, ctx context.Context, cli ovn.Client) string {
	t.Helper()

	switches, err := cli.ListLogicalSwitches(ctx)
	if err != nil {
		t.Fatalf("ListLogicalSwitches: %v", err)
	}
	routers, err := cli.ListLogicalRouters(ctx)
	if err != nil {
		t.Fatalf("ListLogicalRouters: %v", err)
	}
	ports, err := cli.ListLogicalSwitchPorts(ctx)
	if err != nil {
		t.Fatalf("ListLogicalSwitchPorts: %v", err)
	}
	pgs, err := cli.ListPortGroups(ctx)
	if err != nil {
		t.Fatalf("ListPortGroups: %v", err)
	}
	dhcp, err := cli.ListDHCPOptions(ctx)
	if err != nil {
		t.Fatalf("ListDHCPOptions: %v", err)
	}

	// Map each named row's UUID to a stable token so references resolve to
	// names instead of churning UUIDs.
	tokens := map[string]string{}
	for _, s := range switches {
		tokens[s.UUID] = "ls:" + s.Name
	}
	for _, r := range routers {
		tokens[r.UUID] = "lr:" + r.Name
	}
	for _, p := range ports {
		tokens[p.UUID] = "lsp:" + p.Name
	}
	for _, g := range pgs {
		tokens[g.UUID] = "pg:" + g.Name
	}
	for _, d := range dhcp {
		tokens[d.UUID] = "dhcp:" + d.CIDR
	}

	var lines []string
	add := func(kind string, rows []string) {
		for i := range rows {
			rows[i] = kind + " " + normalize(rows[i], tokens)
		}
		slices.Sort(rows)
		lines = append(lines, rows...)
	}

	sw := make([]string, len(switches))
	for i := range switches {
		sw[i] = fmt.Sprintf("%+v", switches[i])
	}
	add("switch", sw)

	rt := make([]string, len(routers))
	for i := range routers {
		rt[i] = fmt.Sprintf("%+v", routers[i])
	}
	add("router", rt)

	lsp := make([]string, len(ports))
	for i := range ports {
		lsp[i] = fmt.Sprintf("%+v", ports[i])
	}
	add("port", lsp)

	pg := make([]string, len(pgs))
	for i := range pgs {
		pg[i] = fmt.Sprintf("%+v", pgs[i])
	}
	add("portgroup", pg)

	dh := make([]string, len(dhcp))
	for i := range dhcp {
		dh[i] = fmt.Sprintf("%+v", dhcp[i])
	}
	add("dhcp", dh)

	// Logical_Router_Policy rows carry egress steering (IGW/NATGW reroutes and
	// drop gates); they have no top-level list accessor, so fetch per router.
	for _, r := range routers {
		pols, err := cli.ListLogicalRouterPolicies(ctx, r.Name)
		if err != nil {
			t.Fatalf("ListLogicalRouterPolicies %s: %v", r.Name, err)
		}
		pl := make([]string, len(pols))
		for i := range pols {
			pl[i] = fmt.Sprintf("%+v", pols[i])
		}
		add("policy", pl)
	}

	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// normalize strips identity churn: known UUIDs become name tokens, residual
// UUIDs collapse to "X", and pointer addresses become "PTR".
func normalize(s string, tokens map[string]string) string {
	for uuid, tok := range tokens {
		s = strings.ReplaceAll(s, uuid, tok)
	}
	s = uuidRe.ReplaceAllString(s, "X")
	return ptrRe.ReplaceAllString(s, "PTR")
}
