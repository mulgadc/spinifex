package reconcile_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// hostRoutes is one node's host route table.
type hostRoutes struct {
	mu     sync.Mutex
	routes map[string]string
	lists  int
}

func newHostRoutes(seed map[string]string) *hostRoutes {
	r := &hostRoutes{routes: map[string]string{}}
	maps.Copy(r.routes, seed)
	return r
}

func (h *hostRoutes) List(context.Context) (map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lists++
	return maps.Clone(h.routes), nil
}

func (h *hostRoutes) Ensure(_ context.Context, cidr, via string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[cidr] = via
	return nil
}

func (h *hostRoutes) Remove(_ context.Context, cidr string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.routes, cidr)
	return nil
}

func (h *hostRoutes) listCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lists
}

func (h *hostRoutes) snapshot() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.routes)
}

// clusterIntent is the control-plane store every node reads.
type clusterIntent struct {
	mu   sync.Mutex
	vpcs map[string]topology.VPCSpec
	igws map[string]external.IGWSpec
	err  error
}

func newClusterIntent() *clusterIntent {
	return &clusterIntent{vpcs: map[string]topology.VPCSpec{}, igws: map[string]external.IGWSpec{}}
}

func (c *clusterIntent) addVPC(id, cidr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vpcs[id] = topology.VPCSpec{VPCID: id, CIDR: netip.MustParsePrefix(cidr)}
}

func (c *clusterIntent) attach(vpcID, igwID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.igws[vpcID] = external.IGWSpec{VPCID: vpcID, InternetGatewayID: igwID}
}

func (c *clusterIntent) detach(vpcID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.igws, vpcID)
}

func (c *clusterIntent) load(context.Context) (reconcile.IntentState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return reconcile.IntentState{}, c.err
	}
	return reconcile.IntentState{VPCs: maps.Clone(c.vpcs), IGWs: maps.Clone(c.igws)}, nil
}

// node is one hypervisor's vpcd as far as host ingress goes.
type node struct {
	routes *hostRoutes
	ingest *reconcile.HostIngress
}

func newNode(m *mock.Client, intent *clusterIntent, preferred string, seed map[string]string) *node {
	routes := newHostRoutes(seed)
	return &node{routes: routes, ingest: &reconcile.HostIngress{
		OVN: m, Routes: routes, LoadIntent: intent.load, PreferredVPC: preferred,
	}}
}

func (n *node) pass(t *testing.T) time.Duration {
	t.Helper()
	revisit, err := n.ingest.Pass(t.Context())
	if err != nil {
		t.Fatalf("host ingress pass: %v", err)
	}
	return revisit
}

func seedRouter(t *testing.T, m *mock.Client, vpcID, cidr string) {
	t.Helper()
	if err := m.CreateLogicalRouter(t.Context(), &nbdb.LogicalRouter{
		Name:        topology.VPCRouter(vpcID),
		ExternalIDs: map[string]string{"spinifex:vpc_id": vpcID, "spinifex:cidr": cidr},
	}); err != nil {
		t.Fatalf("seed router %s: %v", vpcID, err)
	}
}

// seedGateway stands in for the gateway LRP an attach creates in NB.
func seedGateway(t *testing.T, m *mock.Client, vpcID, gwIP string) {
	t.Helper()
	if err := m.CreateLogicalRouterPort(t.Context(), topology.VPCRouter(vpcID), &nbdb.LogicalRouterPort{
		Name:        topology.GatewayRouterPort(vpcID),
		Networks:    []string{gwIP + "/24"},
		ExternalIDs: map[string]string{policy.GatewayIPExtIDKey: gwIP},
	}); err != nil {
		t.Fatalf("seed gateway %s: %v", vpcID, err)
	}
}

// routedIGW is the IGW manager on the node the queue group hands an event to.
// Its hooks only nudge, as in vpcd.
func routedIGW(t *testing.T, m *mock.Client, nudges *int) external.IGWManager {
	t.Helper()
	nat, err := policy.NewNATManager(m, policy.NATModeRouted,
		policy.WithSNATExemptSet(policy.NATExemptSetName, []string{"100.127.0.0/24"}))
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	nudge := func(context.Context, string, string, string) error { *nudges++; return nil }
	mgr, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: policy.NewRouteManager(m), NAT: nat,
		Pool: &external.ExternalPoolConfig{
			Name: "nat-transit", Gateway: "100.127.0.1", PrefixLen: 24,
			GwLrpRangeStart: "100.127.0.240", GwLrpRangeEnd: "100.127.0.243",
		},
		Allocator: external.NewStaticRangeAllocator(m),
		Chassis:   []string{"chassis-a"}, NATMode: policy.NATModeRouted,
		RoutedIngress: &external.RoutedIngressHooks{Ensure: nudge, Remove: nudge},
	})
	if err != nil {
		t.Fatalf("NewIGWManager: %v", err)
	}
	return mgr
}

func assertRoutes(t *testing.T, name string, got, want map[string]string) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s routes = %v, want %v", name, got, want)
	}
}

// The attach event reaches one vpcd of the queue group. Every other node must
// still get the route from intent, or a host that neither handled the event
// nor won a drift lease has no path into the VPC until a full resync.
func TestHostIngress_AttachHandledOnOneNodeConvergesEveryNode(t *testing.T) {
	m := mock.New()
	intent := newClusterIntent()
	seedRouter(t, m, "vpc-1", "10.1.0.0/16")
	intent.addVPC("vpc-1", "10.1.0.0/16")
	a := newNode(m, intent, "", nil)
	b := newNode(m, intent, "", nil)

	intent.attach("vpc-1", "igw-1")
	nudges := 0
	if err := routedIGW(t, m, &nudges).AttachIGW(t.Context(), external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}); err != nil {
		t.Fatalf("AttachIGW on node A: %v", err)
	}
	if nudges == 0 {
		t.Fatal("attach on node A never nudged its host ingress loop")
	}

	a.pass(t)
	// Node B saw only the intent change, never the event.
	b.pass(t)
	want := map[string]string{"10.1.0.0/16": "100.127.0.240"}
	assertRoutes(t, "node A", a.routes.snapshot(), want)
	assertRoutes(t, "node B", b.routes.snapshot(), want)
}

// Detach has the same reach as attach, so the route must leave every node, not
// only the one that ran DetachIGW.
func TestHostIngress_DetachPrunesOnEveryNode(t *testing.T) {
	m := mock.New()
	intent := newClusterIntent()
	seedRouter(t, m, "vpc-1", "10.1.0.0/16")
	intent.addVPC("vpc-1", "10.1.0.0/16")
	intent.attach("vpc-1", "igw-1")
	nudges := 0
	mgr := routedIGW(t, m, &nudges)
	if err := mgr.AttachIGW(t.Context(), external.IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}); err != nil {
		t.Fatalf("AttachIGW: %v", err)
	}
	nodes := []*node{newNode(m, intent, "", nil), newNode(m, intent, "", nil), newNode(m, intent, "", nil)}
	for _, n := range nodes {
		n.pass(t)
		assertRoutes(t, "attached", n.routes.snapshot(), map[string]string{"10.1.0.0/16": "100.127.0.240"})
	}

	intent.detach("vpc-1")
	if err := mgr.DetachIGW(t.Context(), "vpc-1"); err != nil {
		t.Fatalf("DetachIGW on node A: %v", err)
	}
	for i, n := range nodes {
		n.pass(t)
		if got := n.routes.snapshot(); len(got) != 0 {
			t.Errorf("node %d kept %v after detach, want no ingress route", i, got)
		}
	}
}

// Every account's default VPC is 172.31.0.0/16, so two attached VPCs contend
// for one host route. Each node must pick the same holder whatever route it
// had before and whichever order it met the VPCs in.
func TestHostIngress_SharedCIDRPicksTheSameHolderOnEveryNode(t *testing.T) {
	cases := []struct {
		name      string
		preferred string
		wantVia   string
	}{
		{"preferred VPC wins over a lower ID", "vpc-b", "100.127.0.17"},
		{"lower VPC ID wins without a preferred VPC", "", "100.127.0.16"},
		{"a preferred VPC outside the contest changes nothing", "vpc-other", "100.127.0.16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mock.New()
			intent := newClusterIntent()
			for vpcID, gw := range map[string]string{"vpc-a": "100.127.0.16", "vpc-b": "100.127.0.17"} {
				seedRouter(t, m, vpcID, "172.31.0.0/16")
				seedGateway(t, m, vpcID, gw)
				intent.addVPC(vpcID, "172.31.0.0/16")
				intent.attach(vpcID, "igw-"+vpcID)
			}
			want := map[string]string{"172.31.0.0/16": tc.wantVia}
			// Seeded with each holder, as if the event for either VPC had
			// landed on that node first; repeated so map order cannot hide a
			// choice that depends on it.
			for _, seed := range []string{"", "100.127.0.16", "100.127.0.17"} {
				for range 20 {
					var initial map[string]string
					if seed != "" {
						initial = map[string]string{"172.31.0.0/16": seed}
					}
					n := newNode(m, intent, tc.preferred, initial)
					n.pass(t)
					assertRoutes(t, "seed "+seed, n.routes.snapshot(), want)
				}
			}
		})
	}
}

// Until the winning VPC's gateway exists the route is left alone: switching it
// to the loser and back would cut the CIDR over twice for nothing.
func TestHostIngress_PendingWinnerHoldsTheRouteAndRevisits(t *testing.T) {
	m := mock.New()
	intent := newClusterIntent()
	seedRouter(t, m, "vpc-a", "172.31.0.0/16")
	seedRouter(t, m, "vpc-b", "172.31.0.0/16")
	seedGateway(t, m, "vpc-b", "100.127.0.17")
	intent.addVPC("vpc-a", "172.31.0.0/16")
	intent.addVPC("vpc-b", "172.31.0.0/16")
	intent.attach("vpc-a", "igw-a")
	intent.attach("vpc-b", "igw-b")
	n := newNode(m, intent, "vpc-a", map[string]string{"172.31.0.0/16": "100.127.0.9"})

	if revisit := n.pass(t); revisit <= 0 {
		t.Errorf("revisit = %v, want a short retry while vpc-a's gateway is missing", revisit)
	}
	assertRoutes(t, "pending", n.routes.snapshot(), map[string]string{"172.31.0.0/16": "100.127.0.9"})

	seedGateway(t, m, "vpc-a", "100.127.0.16")
	if revisit := n.pass(t); revisit != 0 {
		t.Errorf("revisit = %v after convergence, want 0", revisit)
	}
	assertRoutes(t, "converged", n.routes.snapshot(), map[string]string{"172.31.0.0/16": "100.127.0.16"})
}

// A restarted vpcd has no memory of the events it handled. Its first pass must
// rebuild the table from intent: repoint a route left on a stale gateway, add
// one it never had, and prune one whose VPC detached while it was down.
func TestHostIngress_RestartConvergesFromIntent(t *testing.T) {
	m := mock.New()
	intent := newClusterIntent()
	for vpcID, v := range map[string][2]string{
		"vpc-1": {"10.1.0.0/16", "100.127.0.20"},
		"vpc-2": {"10.2.0.0/16", "100.127.0.21"},
	} {
		seedRouter(t, m, vpcID, v[0])
		seedGateway(t, m, vpcID, v[1])
		intent.addVPC(vpcID, v[0])
		intent.attach(vpcID, "igw-"+vpcID)
	}
	n := newNode(m, intent, "", map[string]string{
		"10.1.0.0/16": "100.127.0.99",
		"10.9.0.0/16": "100.127.0.30",
	})

	n.pass(t)
	assertRoutes(t, "after restart", n.routes.snapshot(), map[string]string{
		"10.1.0.0/16": "100.127.0.20",
		"10.2.0.0/16": "100.127.0.21",
	})
}

// An unreadable store is not an empty one: reading it as empty would strip
// every VPC's ingress route from every node at once.
func TestHostIngress_IntentReadFailureNeverPrunes(t *testing.T) {
	m := mock.New()
	intent := newClusterIntent()
	intent.err = errors.New("kv unavailable")
	seed := map[string]string{"10.1.0.0/16": "100.127.0.20"}
	n := newNode(m, intent, "", seed)

	if _, err := n.ingest.Pass(t.Context()); err == nil {
		t.Fatal("expected the intent read failure to be returned")
	}
	assertRoutes(t, "after failed read", n.routes.snapshot(), seed)
}

// The loop has to wake on an intent write by itself, on every node at once,
// while another node holds the drift lease and no event arrives, well inside
// its resync.
func TestRunHostIngress_IntentWriteConvergesEveryNodeWithoutLeaseOrEvent(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	vpcs, err := kvutil.GetOrCreateBucket(ctx, js, ec2vpc.KVBucketVPCs, 1)
	if err != nil {
		t.Fatalf("create VPC bucket: %v", err)
	}
	igws, err := kvutil.GetOrCreateBucket(ctx, js, ec2igw.KVBucketIGW, 1)
	if err != nil {
		t.Fatalf("create IGW bucket: %v", err)
	}
	put(t, vpcs, "acct/vpc-1", ec2vpc.VPCRecord{VpcId: "vpc-1", CidrBlock: "10.1.0.0/16"})
	release, elected := reconcile.AcquireLeader(ctx, nc, reconcile.KVBucketVPCDReconcile, "node-elsewhere")
	if !elected {
		t.Fatal("could not take the drift lease for another node")
	}
	t.Cleanup(release)

	m := mock.New()
	seedRouter(t, m, "vpc-1", "10.1.0.0/16")
	seedGateway(t, m, "vpc-1", "100.127.0.20")
	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	nodes := make([]*hostRoutes, 3)
	for i := range nodes {
		nodes[i] = newHostRoutes(nil)
		h := &reconcile.HostIngress{
			OVN: m, Routes: nodes[i], Resync: time.Minute,
			LoadIntent: func(ctx context.Context) (reconcile.IntentState, error) {
				return reconcile.LoadIntentFromKV(ctx, js, "us-east-1a")
			},
		}
		wg.Go(func() { reconcile.RunHostIngress(loopCtx, js, h, nil) })
	}
	// The startup pass has to be over before the attach is written, or it could
	// be what converged a node rather than the watch.
	for _, n := range nodes {
		for n.listCount() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	want := map[string]string{"10.1.0.0/16": "100.127.0.20"}
	converged := func() bool {
		for _, n := range nodes {
			if !maps.Equal(n.snapshot(), want) {
				return false
			}
		}
		return true
	}
	// Watchers attach asynchronously and see updates only, so the write is
	// repeated until one lands rather than racing the loop's startup.
	deadline := time.Now().Add(10 * time.Second)
	for !converged() {
		if time.Now().After(deadline) {
			for i, n := range nodes {
				t.Errorf("node %d routes = %v 10s after the attach was written, want %v", i, n.snapshot(), want)
			}
			t.FailNow()
		}
		put(t, igws, "acct/igw-1", ec2igw.IGWRecord{InternetGatewayId: "igw-1", VpcId: "vpc-1", State: "available"})
		time.Sleep(500 * time.Millisecond)
	}
}

func put(t *testing.T, kv jetstream.KeyValue, key string, rec any) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	if _, err := kv.Put(t.Context(), key, b); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}
