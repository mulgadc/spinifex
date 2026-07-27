package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
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

	jsm, err := NewJetStreamManager(nc, 1)
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

// --- Cluster-safety coverage (mulga-f3j2x) ---

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
	jsm, err := NewJetStreamManager(nc, 1)
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
