package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
)

// newTestMgmtJSM returns a JetStreamManager backed by the shared per-package
// JetStream test server, with the cluster-state bucket initialised — the
// same bucket UpdateMgmtIPAM writes into (namespaced under "mgmt-ipam.").
func newTestMgmtJSM(t *testing.T) *JetStreamManager {
	t.Helper()
	nc, err := nats.Connect(sharedJSNATSURL)
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(nc.Close)

	jsm, err := NewJetStreamManager(nc)
	if err != nil {
		t.Fatalf("new JetStreamManager: %v", err)
	}
	if err := jsm.InitClusterStateBucket(); err != nil {
		t.Fatalf("init cluster state bucket: %v", err)
	}
	return jsm
}

// cleanupMgmtIPAM deletes the mgmt-ipam record for a's subnet at test end.
// The cluster-state bucket is shared by the whole package's test binary, so
// without this a record left behind by one test (or one -count repetition)
// would leak into the next test/run that reuses the same bridge subnet.
func cleanupMgmtIPAM(t *testing.T, jsm *JetStreamManager, a *MgmtIPAllocator) {
	t.Helper()
	t.Cleanup(func() {
		_ = jsm.clusterKV.Delete(context.Background(), mgmtIPAMKeyPrefix+a.subnet)
	})
}

func TestNewMgmtIPAllocator(t *testing.T) {
	tests := []struct {
		name      string
		bridgeIP  string
		wantErr   bool
		wantBase3 byte // expected 4th octet of base (always 0)
	}{
		{"valid", "10.15.8.1", false, 0},
		{"different subnet", "192.168.1.33", false, 0},
		{"invalid", "not-an-ip", true, 0},
		{"ipv6", "::1", true, 0},
		{"empty", "", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewMgmtIPAllocator(tt.bridgeIP)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a.baseIP[3] != tt.wantBase3 {
				t.Errorf("base[3] = %d, want %d", a.baseIP[3], tt.wantBase3)
			}
		})
	}
}

// TestMgmtIPAllocator_Allocate exercises the same scan-from-.10 contract as
// before the KV rewrite, but now through a bound cluster KV — Allocate
// refuses new addresses without one (see fail-closed tests below), so every
// allocator under test here binds one first.
func TestMgmtIPAllocator_Allocate(t *testing.T) {
	tests := []struct {
		name     string
		bridgeIP string
		firstIP  string
		secondIP string
	}{
		{"primary subnet", "10.15.8.1", "10.15.8.10", "10.15.8.11"},
		{"different subnet", "192.168.1.33", "192.168.1.10", "192.168.1.11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsm := newTestMgmtJSM(t)
			a, err := NewMgmtIPAllocator(tt.bridgeIP)
			if err != nil {
				t.Fatal(err)
			}
			a.BindKV(jsm, "node-a")
			cleanupMgmtIPAM(t, jsm, a)

			ip, err := a.Allocate("i-first")
			if err != nil {
				t.Fatal(err)
			}
			if ip != tt.firstIP {
				t.Errorf("first IP = %q, want %q", ip, tt.firstIP)
			}

			ip, err = a.Allocate("i-second")
			if err != nil {
				t.Fatal(err)
			}
			if ip != tt.secondIP {
				t.Errorf("second IP = %q, want %q", ip, tt.secondIP)
			}

			// Re-allocating same instance returns same IP
			ip, err = a.Allocate("i-first")
			if err != nil {
				t.Fatal(err)
			}
			if ip != tt.firstIP {
				t.Errorf("re-allocate = %q, want %q", ip, tt.firstIP)
			}

			if a.AllocatedCount() != 2 {
				t.Errorf("count = %d, want 2", a.AllocatedCount())
			}
		})
	}
}

func TestMgmtIPAllocator_Release(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a, err := NewMgmtIPAllocator("10.16.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	a.Allocate("i-one")
	a.Allocate("i-two")
	a.Release("i-one")

	if a.AllocatedCount() != 1 {
		t.Errorf("count after release = %d, want 1", a.AllocatedCount())
	}

	// Released IP should be reused
	ip, err := a.Allocate("i-three")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.16.8.10" {
		t.Errorf("reused IP = %q, want 10.16.8.10", ip)
	}
}

// TestMgmtIPAllocator_ReleaseNonexistent asserts Release never panics or
// blocks on an instance ID with no allocation — including with no KV bound
// at all, the shape startLocal constructs.
func TestMgmtIPAllocator_ReleaseNonexistent(t *testing.T) {
	a, err := NewMgmtIPAllocator("10.15.8.1")
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic
	a.Release("i-nonexistent")
}

func TestMgmtIPAllocator_Exhaustion(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a, err := NewMgmtIPAllocator("10.17.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	// Fill all 240 slots (.10-.249)
	for i := range 240 {
		_, err := a.Allocate(fmt.Sprintf("i-%d", i))
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
	}

	// Next should fail, naming the cluster rather than a host-local limit.
	_, err = a.Allocate("i-overflow")
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !strings.Contains(err.Error(), "exhausted across the cluster") {
		t.Errorf("exhaustion error = %q, want it to name the cluster", err.Error())
	}

	// Release one, then it should work
	a.Release("i-0")
	ip, err := a.Allocate("i-overflow")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if ip != "10.17.8.10" {
		t.Errorf("reused IP = %q, want 10.17.8.10", ip)
	}
}

// TestMgmtIPAllocator_Rebuild binds KV before Rebuild so it exercises the
// reconcile path (CAS-inserting local VMs into the shared record), not just
// the local-cache refresh that runs unbound in startLocal.
func TestMgmtIPAllocator_Rebuild(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a, err := NewMgmtIPAllocator("10.18.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	vms := map[string]*vm.VM{
		"i-a": {MgmtIP: "10.18.8.10"},
		"i-b": {MgmtIP: "10.18.8.15"},
		"i-c": {MgmtIP: ""}, // no mgmt NIC
		"i-d": {MgmtIP: "10.18.8.20"},
	}

	a.Rebuild(vms)

	if a.AllocatedCount() != 3 {
		t.Errorf("count after rebuild = %d, want 3", a.AllocatedCount())
	}

	// Next allocation should skip the already-used IPs
	ip, err := a.Allocate("i-new")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.18.8.11" {
		t.Errorf("next IP after rebuild = %q, want 10.18.8.11", ip)
	}
}

// TestMgmtIPAllocator_Rebuild_UnboundIsCacheOnly is the startLocal shape:
// Rebuild with no KV bound must only refresh the local cache and must never
// panic or block waiting on a cluster that doesn't exist yet.
func TestMgmtIPAllocator_Rebuild_UnboundIsCacheOnly(t *testing.T) {
	a, err := NewMgmtIPAllocator("10.19.8.1")
	if err != nil {
		t.Fatal(err)
	}

	vms := map[string]*vm.VM{
		"i-a": {MgmtIP: "10.19.8.10"},
	}
	a.Rebuild(vms)

	if a.AllocatedCount() != 1 {
		t.Errorf("count after unbound rebuild = %d, want 1", a.AllocatedCount())
	}
	// A cached ID still resolves without KV.
	ip, err := a.Allocate("i-a")
	if err != nil {
		t.Fatalf("cached instance should resolve without KV: %v", err)
	}
	if ip != "10.19.8.10" {
		t.Errorf("cached IP = %q, want 10.19.8.10", ip)
	}
}

func TestMgmtIPAllocator_Concurrent(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a, err := NewMgmtIPAllocator("10.20.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	var wg sync.WaitGroup
	results := make(map[string]string)
	var mu sync.Mutex
	errs := make([]error, 0)

	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("i-%d", n)
			ip, err := a.Allocate(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results[id] = ip
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("allocation errors: %v", errs)
	}
	if len(results) != 50 {
		t.Errorf("got %d results, want 50", len(results))
	}

	// All IPs should be unique
	seen := make(map[string]bool)
	for id, ip := range results {
		if seen[ip] {
			t.Errorf("duplicate IP %s for %s", ip, id)
		}
		seen[ip] = true
	}
}

// --- Cluster-safety coverage ---

// TestMgmtIPAllocator_SharedKV_NoDuplicates is the core regression test:
// two allocators standing in for two nodes, sharing one KV, must never both
// hand out the same address even though each scans from .10 independently.
func TestMgmtIPAllocator_SharedKV_NoDuplicates(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.21.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.21.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	seen := make(map[string]string) // ip -> owning instance
	allocate := func(a *MgmtIPAllocator, id string) {
		ip, err := a.Allocate(id)
		if err != nil {
			t.Fatalf("allocate %s: %v", id, err)
		}
		if owner, dup := seen[ip]; dup {
			t.Fatalf("duplicate address %s: held by %s and %s", ip, owner, id)
		}
		seen[ip] = id
	}

	// Alternate allocation across the two "nodes" — each locally believes it
	// is scanning a fresh range from .10, but the shared KV record must
	// still serialise them onto distinct addresses.
	for i := range 10 {
		allocate(node1, fmt.Sprintf("node1-i-%d", i))
		allocate(node2, fmt.Sprintf("node2-i-%d", i))
	}

	if len(seen) != 20 {
		t.Errorf("got %d unique addresses, want 20", len(seen))
	}
}

// TestMgmtIPAllocator_ConcurrentAcrossNodes fires concurrent allocations
// from three simulated nodes against the same KV record, forcing genuine
// CAS conflicts (not just the happy path) as their Get/mutate/Update
// windows overlap. Every address handed out across all three must be
// unique — this is the exact shape of CI run 29912697556, where independent
// per-node allocation raced onto the same br-mgmt address.
func TestMgmtIPAllocator_ConcurrentAcrossNodes(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	const nodeCount = 3
	const perNode = 20
	nodes := make([]*MgmtIPAllocator, nodeCount)
	for n := range nodeCount {
		a, err := NewMgmtIPAllocator("10.22.8.1")
		if err != nil {
			t.Fatal(err)
		}
		a.BindKV(jsm, fmt.Sprintf("node-%d", n))
		if n == 0 {
			cleanupMgmtIPAM(t, jsm, a)
		}
		nodes[n] = a
	}

	type result struct {
		id  string
		ip  string
		err error
	}
	results := make(chan result, nodeCount*perNode)
	var wg sync.WaitGroup
	for n := range nodeCount {
		for i := range perNode {
			wg.Add(1)
			go func(n, i int) {
				defer wg.Done()
				id := fmt.Sprintf("node%d-i-%d", n, i)
				ip, err := nodes[n].Allocate(id)
				results <- result{id: id, ip: ip, err: err}
			}(n, i)
		}
	}
	wg.Wait()
	close(results)

	seen := make(map[string]string)
	for r := range results {
		if r.err != nil {
			t.Fatalf("allocate %s: %v", r.id, r.err)
		}
		if owner, dup := seen[r.ip]; dup {
			t.Fatalf("duplicate address %s: held by %s and %s", r.ip, owner, r.id)
		}
		seen[r.ip] = r.id
	}
	if len(seen) != nodeCount*perNode {
		t.Errorf("got %d unique addresses, want %d", len(seen), nodeCount*perNode)
	}
}

// TestMgmtIPAllocator_Allocate_IdempotentAcrossNodes proves idempotency is a
// property of the shared KV record, not of one allocator's local cache: a
// second allocator (standing in for the instance's owning node restarting
// inside a partition and losing its cache) re-requesting a known instance ID
// gets back the same address rather than a new one.
func TestMgmtIPAllocator_Allocate_IdempotentAcrossNodes(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.23.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	ip, err := node1.Allocate("i-reattach")
	if err != nil {
		t.Fatal(err)
	}

	// A second allocator, simulating a fresh process on the same node after
	// a restart with an empty local cache.
	node1Restarted, err := NewMgmtIPAllocator("10.23.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1Restarted.BindKV(jsm, "node-1")

	ip2, err := node1Restarted.Allocate("i-reattach")
	if err != nil {
		t.Fatal(err)
	}
	if ip2 != ip {
		t.Errorf("re-allocate from fresh allocator = %q, want %q (idempotent by instance ID)", ip2, ip)
	}
}

// TestMgmtIPAllocator_Release_FreesAddressClusterWide proves Release's
// effect is visible cluster-wide: a second allocator, not the one that
// released the address, can immediately take it.
func TestMgmtIPAllocator_Release_FreesAddressClusterWide(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.24.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.24.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	ip, err := node1.Allocate("i-node1-only")
	if err != nil {
		t.Fatal(err)
	}
	node1.Release("i-node1-only")

	// node2 must now be able to take the address node1 just released.
	ip2, err := node2.Allocate("i-node2-new")
	if err != nil {
		t.Fatal(err)
	}
	if ip2 != ip {
		t.Errorf("node2 got %q, want the released address %q", ip2, ip)
	}
}

// TestMgmtIPAllocator_Exhaustion_ClusterWide proves the exhaustion check is
// against the cluster-wide record, not this allocator's own local cache:
// two allocators together hold all 240 addresses (120 each, so neither
// locally believes the range is full) and a third allocation from either
// must still fail.
func TestMgmtIPAllocator_Exhaustion_ClusterWide(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.25.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.25.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	for i := range 120 {
		if _, err := node1.Allocate(fmt.Sprintf("node1-i-%d", i)); err != nil {
			t.Fatalf("node1 allocation %d failed: %v", i, err)
		}
	}
	for i := range 120 {
		if _, err := node2.Allocate(fmt.Sprintf("node2-i-%d", i)); err != nil {
			t.Fatalf("node2 allocation %d failed: %v", i, err)
		}
	}

	// Each allocator's local cache only knows about its own 120 — but the
	// cluster-wide record holds all 240, so the next allocation from either
	// must fail.
	if node1.AllocatedCount() != 120 {
		t.Fatalf("node1 local cache = %d, want 120 (proves the check isn't local)", node1.AllocatedCount())
	}
	if _, err := node1.Allocate("i-overflow-from-node1"); err == nil {
		t.Fatal("expected exhaustion error from node1")
	}
	if _, err := node2.Allocate("i-overflow-from-node2"); err == nil {
		t.Fatal("expected exhaustion error from node2")
	}
}

// TestMgmtIPAllocator_Allocate_RefusedWhenKVUnhealthy is the fail-closed
// contract: once KV is unreachable, a brand new allocation is refused, but
// an instance ID already resolved (and cached) before the outage keeps
// resolving from the local cache without touching KV at all.
func TestMgmtIPAllocator_Allocate_RefusedWhenKVUnhealthy(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	if err != nil {
		t.Fatal(err)
	}
	jsm, err := NewJetStreamManager(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := jsm.InitClusterStateBucket(); err != nil {
		t.Fatal(err)
	}

	a, err := NewMgmtIPAllocator("10.26.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-1")

	ip, err := a.Allocate("i-known")
	if err != nil {
		t.Fatal(err)
	}

	// Sever the connection backing this JetStreamManager: KVHealthy's
	// AccountInfo round-trip will now fail, simulating a partition.
	nc.Close()

	if jsm.KVHealthy() {
		t.Fatal("expected KVHealthy to report false after closing the connection")
	}

	// A known instance ID still resolves — served entirely from the local
	// cache, no KV round-trip required.
	cachedIP, err := a.Allocate("i-known")
	if err != nil {
		t.Fatalf("cached instance should resolve without KV: %v", err)
	}
	if cachedIP != ip {
		t.Errorf("cached IP = %q, want %q", cachedIP, ip)
	}

	// A brand new allocation must be refused rather than risk a duplicate
	// against a cluster-wide record this node can no longer trust.
	if _, err := a.Allocate("i-new-during-partition"); err == nil {
		t.Fatal("expected allocation to be refused while KV is unhealthy")
	}
}

// TestMgmtIPAllocator_Allocate_RefusedWhenKVNeverBound is the startLocal
// shape: an allocator with no KV bound at all (as constructed before
// startCluster runs) must refuse new allocations exactly like an unhealthy
// bound KV, while still serving whatever Rebuild already cached.
func TestMgmtIPAllocator_Allocate_RefusedWhenKVNeverBound(t *testing.T) {
	a, err := NewMgmtIPAllocator("10.27.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.Rebuild(map[string]*vm.VM{"i-known": {MgmtIP: "10.27.8.10"}})

	ip, err := a.Allocate("i-known")
	if err != nil {
		t.Fatalf("cached instance should resolve without KV: %v", err)
	}
	if ip != "10.27.8.10" {
		t.Errorf("cached IP = %q, want 10.27.8.10", ip)
	}

	if _, err := a.Allocate("i-brand-new"); err == nil {
		t.Fatal("expected allocation to be refused with no KV bound")
	}
}

// recordingPlumber satisfies vm.NetworkPlumber for the cleanup-path tests. Only
// CleanupTap is exercised; the rest are here to satisfy the interface.
type recordingPlumber struct {
	cleanedTaps []string
}

var _ vm.NetworkPlumber = (*recordingPlumber)(nil)

func (p *recordingPlumber) SetupTap(vm.TapSpec) error { return nil }
func (p *recordingPlumber) CleanupTap(name string) error {
	p.cleanedTaps = append(p.cleanedTaps, name)
	return nil
}
func (p *recordingPlumber) EnsureIMDSDatapathBridge() error         { return nil }
func (p *recordingPlumber) AttachIMDSDatapath(_, _, _ string) error { return nil }
func (p *recordingPlumber) DetachIMDSDatapath(string) error         { return nil }
func (p *recordingPlumber) EnsureVPCHostPort(_, _, _ string) error  { return nil }
func (p *recordingPlumber) RemoveVPCHostPort(string) error          { return nil }

// neighCall is one recorded flush or prime.
type neighCall struct {
	dev string
	ip  string
	mac string
}

// captureNeighHooks swaps the kernel neighbour hooks for recorders and restores
// them when the test ends.
func captureNeighHooks(t *testing.T) (flushed, primed *[]neighCall) {
	t.Helper()
	var f, p []neighCall
	origFlush, origPrime := flushMgmtNeigh, primeMgmtNeigh
	flushMgmtNeigh = func(_ context.Context, _ host.Runner, dev, ip string) error {
		f = append(f, neighCall{dev: dev, ip: ip})
		return nil
	}
	primeMgmtNeigh = func(_ context.Context, _ host.Runner, dev, ip, mac string) error {
		p = append(p, neighCall{dev: dev, ip: ip, mac: mac})
		return nil
	}
	t.Cleanup(func() { flushMgmtNeigh, primeMgmtNeigh = origFlush, origPrime })
	return &f, &p
}

// A released address is re-handed within seconds with a different MAC, so the
// stale entry has to go or the next holder blackholes until the kernel expires
// it. This is the defect behind the nightly rds isolation failure.
func TestCleanupMgmtNetwork_FlushesNeighForReleasedAddress(t *testing.T) {
	flushed, _ := captureNeighHooks(t)

	alloc, err := NewMgmtIPAllocator("10.15.8.1")
	if err != nil {
		t.Fatal(err)
	}
	plumber := &recordingPlumber{}
	d := &Daemon{networkPlumber: plumber, mgmtIPAllocator: alloc, mgmtBridgeIP: "10.15.8.1"}

	newInstanceCleanerAdapter(d).CleanupMgmtNetwork(&vm.VM{ID: "i-abc", MgmtIP: "10.15.8.10"})

	if len(*flushed) != 1 {
		t.Fatalf("flush calls = %d, want 1", len(*flushed))
	}
	if got := (*flushed)[0]; got.ip != "10.15.8.10" || got.dev != "br-mgmt" {
		t.Errorf("flushed %+v, want ip 10.15.8.10 on br-mgmt", got)
	}
	if len(plumber.cleanedTaps) != 1 {
		t.Errorf("tap cleanup calls = %d, want 1", len(plumber.cleanedTaps))
	}
}

// An instance that never got a mgmt NIC has no address to invalidate, and
// flushing "" would be an error the operator cannot act on.
func TestCleanupMgmtNetwork_NoMgmtIPSkipsFlush(t *testing.T) {
	flushed, _ := captureNeighHooks(t)

	d := &Daemon{networkPlumber: &recordingPlumber{}}
	newInstanceCleanerAdapter(d).CleanupMgmtNetwork(&vm.VM{ID: "i-abc"})

	if len(*flushed) != 0 {
		t.Errorf("flush calls = %d, want 0", len(*flushed))
	}
}

func TestInvalidateMgmtNeigh_UsesConfiguredBridge(t *testing.T) {
	flushed, _ := captureNeighHooks(t)

	d := &Daemon{config: &config.Config{}, mgmtBridgeIP: "10.15.8.1"}
	d.config.Daemon.MgmtBridge = "br-ctrl"
	d.invalidateMgmtNeigh("i-abc", "10.15.8.10")

	if len(*flushed) != 1 || (*flushed)[0].dev != "br-ctrl" {
		t.Fatalf("flushed %+v, want one call on br-ctrl", *flushed)
	}
}

// startLocal tolerates an absent bridge, and every persisted instance still
// carries a mgmt IP. Flushing against a device that is not there would warn
// about a blackhole that cannot happen, on every terminate.
func TestInvalidateMgmtNeigh_NoBridgeIsNoOp(t *testing.T) {
	flushed, _ := captureNeighHooks(t)

	d := &Daemon{}
	d.invalidateMgmtNeigh("i-abc", "10.15.8.10")

	if len(*flushed) != 0 {
		t.Errorf("flush calls = %d, want 0", len(*flushed))
	}
}

// The MAC is known at attach time, so the entry is programmed directly rather
// than left for the first dial to resolve — the same choice the EIP path makes.
func TestPrimeMgmtNeighEntry_ProgramsAddressAndMAC(t *testing.T) {
	_, primed := captureNeighHooks(t)

	d := &Daemon{}
	d.primeMgmtNeighEntry("i-abc", "10.15.8.10", "02:d9:f3:a8:fb:be")

	if len(*primed) != 1 {
		t.Fatalf("prime calls = %d, want 1", len(*primed))
	}
	want := neighCall{dev: "br-mgmt", ip: "10.15.8.10", mac: "02:d9:f3:a8:fb:be"}
	if (*primed)[0] != want {
		t.Errorf("primed %+v, want %+v", (*primed)[0], want)
	}
}

func TestPrimeMgmtNeighEntry_NoMACIsNoOp(t *testing.T) {
	_, primed := captureNeighHooks(t)

	d := &Daemon{}
	d.primeMgmtNeighEntry("i-abc", "10.15.8.10", "")

	if len(*primed) != 0 {
		t.Errorf("prime calls = %d, want 0", len(*primed))
	}
}

// mgmtIPEntryFor reads the cluster record's entry for one instance, so a test
// can assert who is recorded as holding an address rather than inferring it from
// whether the next allocation happens to collide.
func mgmtIPEntryFor(t *testing.T, jsm *JetStreamManager, a *MgmtIPAllocator, instanceID string) (MgmtIPEntry, bool) {
	t.Helper()
	record, err := updateMgmtIPAMWithRetry(jsm, a.subnet, func(*MgmtIPRecord) {}, false)
	if err != nil {
		t.Fatalf("read mgmt ipam record: %v", err)
	}
	for _, e := range record.Allocated {
		if e.InstanceID == instanceID {
			return e, true
		}
	}
	return MgmtIPEntry{}, false
}

// The node an instance left runs the teardown for it, and br-mgmt is one flat L2
// segment across the cluster. A release there would put an address still in use
// back in the pool for any node's next instance, so only the node the entry
// names may free it.
func TestMgmtIPAllocator_ReleaseWillNotFreeAnAddressAnotherNodeHolds(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.31.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.31.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	ip, err := node1.Allocate("i-moved")
	if err != nil {
		t.Fatal(err)
	}

	// The instance is recovered onto node2, which takes the reservation over.
	node2.Claim("i-moved", ip)

	// node1 then reaches its own teardown for the copy it is dropping.
	node1.Release("i-moved")

	entry, present := mgmtIPEntryFor(t, jsm, node1, "i-moved")
	if !present {
		t.Fatalf("node-1 freed %s while node-2 was running the instance; "+
			"on a flat segment the next instance anywhere can now be given an address in use", ip)
	}
	if entry.Node != "node-2" || entry.IP != ip {
		t.Errorf("entry after the takeover = %+v, want node-2 holding %s", entry, ip)
	}
}

// Without the claim the entry keeps naming the node the instance left, so the
// node actually running it could never give the address back and it would stay
// reserved for good.
func TestMgmtIPAllocator_ClaimLetsTheNewHolderReleaseItLater(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.32.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.32.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	ip, err := node1.Allocate("i-moved")
	if err != nil {
		t.Fatal(err)
	}

	node2.Claim("i-moved", ip)
	node2.Release("i-moved")

	if entry, present := mgmtIPEntryFor(t, jsm, node1, "i-moved"); present {
		t.Errorf("the holder could not release its own address: entry still %+v", entry)
	}

	// And the address is genuinely back, not merely unrecorded.
	reused, err := node1.Allocate("i-next")
	if err != nil {
		t.Fatal(err)
	}
	if reused != ip {
		t.Errorf("next allocation = %q, want the freed address %q", reused, ip)
	}
}

// Claim is idempotent and does not allocate: an instance carrying an address
// nothing wrote down still holds it on the segment, so the record has to say so.
func TestMgmtIPAllocator_ClaimRecordsAnAddressWithNoEntry(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	a, err := NewMgmtIPAllocator("10.33.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	a.Claim("i-orphan", "10.33.8.77")
	a.Claim("i-orphan", "10.33.8.77")

	entry, present := mgmtIPEntryFor(t, jsm, a, "i-orphan")
	if !present {
		t.Fatal("an address in use on the segment must be recorded, or another instance is given it")
	}
	if entry.IP != "10.33.8.77" || entry.Node != "node-a" {
		t.Errorf("entry = %+v, want node-a holding 10.33.8.77", entry)
	}

	// Allocate must now route around it rather than handing it out again.
	ip, err := a.Allocate("i-next")
	if err != nil {
		t.Fatal(err)
	}
	if ip == "10.33.8.77" {
		t.Error("Allocate handed out an address Claim had already recorded as in use")
	}
}

// A record written before the node field existed names nobody. Refusing those
// would leak every pre-upgrade address forever, which is worse than the
// collision the check exists to prevent — there is no other holder to collide
// with.
func TestMgmtIPAllocator_ReleaseFreesAnEntryThatNamesNoNode(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	a, err := NewMgmtIPAllocator("10.34.8.1")
	if err != nil {
		t.Fatal(err)
	}
	a.BindKV(jsm, "node-a")
	cleanupMgmtIPAM(t, jsm, a)

	if _, err := updateMgmtIPAMWithRetry(jsm, a.subnet, func(r *MgmtIPRecord) {
		r.Subnet = a.subnet
		r.Allocated = append(r.Allocated, MgmtIPEntry{IP: "10.34.8.10", InstanceID: "i-legacy"})
	}, true); err != nil {
		t.Fatal(err)
	}

	a.Release("i-legacy")

	if entry, present := mgmtIPEntryFor(t, jsm, a, "i-legacy"); present {
		t.Errorf("a pre-upgrade entry was left reserved forever: %+v", entry)
	}
}

// Rebuild runs on a node that is already running the instance, so it is the
// other place the record can be told the address moved — the case where the
// recovery happened while this node was down.
func TestMgmtIPAllocator_RebuildTakesOverAnEntryFromAnotherNode(t *testing.T) {
	jsm := newTestMgmtJSM(t)

	node1, err := NewMgmtIPAllocator("10.35.8.1")
	if err != nil {
		t.Fatal(err)
	}
	node1.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, node1)

	node2, err := NewMgmtIPAllocator("10.35.8.2")
	if err != nil {
		t.Fatal(err)
	}
	node2.BindKV(jsm, "node-2")

	ip, err := node1.Allocate("i-moved")
	if err != nil {
		t.Fatal(err)
	}

	node2.Rebuild(map[string]*vm.VM{"i-moved": {MgmtIP: ip}})

	entry, present := mgmtIPEntryFor(t, jsm, node1, "i-moved")
	if !present {
		t.Fatal("the entry vanished; Rebuild must re-stamp it, not remove it")
	}
	if entry.Node != "node-2" {
		t.Errorf("holder after rebuild = %q, want node-2, or nothing can ever release this address", entry.Node)
	}
}
