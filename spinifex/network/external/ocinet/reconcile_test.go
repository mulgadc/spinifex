package ocinet_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/ocinet"
)

// A crash between creating the OCI objects and writing the binding leaves a
// reserved public IP that bills and holds one of the 50 per-region slots, with
// nothing referencing it. Reconcile is the only thing that can ever find it.
func TestReconcileCollectsALeakedPairFromAnInterruptedAllocate(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	// Simulate the crash: the objects exist and carry our prefix, but the
	// binding was never written.
	priv, err := fake.AssignPrivateIP(ctx, "ocid1.vnic.oc1..vnic1", netip.Addr{}, ocinet.DisplayNamePrefix+"eipalloc-lost")
	require.NoError(t, err)
	_, err = fake.CreatePublicIP(ctx, "ocid1.compartment.oc1..comp1", priv.ID, ocinet.DisplayNamePrefix+"eipalloc-lost")
	require.NoError(t, err)
	require.Len(t, fake.PublicIPs(), 1)

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{priv.ID}, res.Collected)
	assert.Empty(t, fake.PrivateIPs(), "leaked private IP was not collected")
	assert.Empty(t, fake.PublicIPs(), "leaked reserved public IP was not collected — this one bills")

	// The leak this pass collects is an OCI pair created before its binding was
	// ever written, so the pool still holds no record afterwards.
	_, err = store.Get(ctx, "oci-wan")
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

// The safety property of the whole pass. mulga-poc carries operator-created
// secondary addresses on the same VNIC that Spinifex allocates from, and a
// reconcile that collected those would take the node's own networking down.
func TestReconcileNeverTouchesAddressesThatAreNotOurs(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, _ := newTestAllocator(t, fake)

	// The operator's own: the VNIC's primary, and a hand-made secondary.
	fake.SeedPrivateIP(oci.PrivateIP{
		ID: "ocid1.privateip.oc1..primary", Address: netip.MustParseAddr("10.200.0.29"),
		VNICID: "ocid1.vnic.oc1..vnic1", DisplayName: "vnic-1", IsPrimary: true,
	})
	fake.SeedPrivateIP(oci.PrivateIP{
		ID: "ocid1.privateip.oc1..operator", Address: netip.MustParseAddr("10.200.0.81"),
		VNICID: "ocid1.vnic.oc1..vnic1", DisplayName: "vnic-v3",
	})

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, res.Collected, "reconcile collected an address it did not create")
	assert.Equal(t, 2, res.Skipped)
	assert.Len(t, fake.PrivateIPs(), 2, "operator addresses must survive a reconcile pass")
}

// An interrupted Release, or someone deleting the address in the OCI console,
// leaves a binding naming objects that no longer exist. Left in place it would
// be reported to a customer as their address.
func TestReconcileDropsABindingWhoseObjectsAreGone(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	ip, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	// Remove the objects behind our back.
	for _, p := range fake.PrivateIPs() {
		require.NoError(t, fake.UnassignPrivateIP(ctx, p.ID))
	}
	for _, p := range fake.PublicIPs() {
		require.NoError(t, fake.DeletePublicIP(ctx, p.ID))
	}

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{ip.String()}, res.Stale)
	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Empty(t, rec.Bindings, "stale binding survived reconcile")
}

func TestReconcileLeavesAHealthyAllocationAlone(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	ip, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, res.Collected)
	assert.Empty(t, res.Stale)
	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Contains(t, rec.Bindings, ip.String())
	assert.Len(t, fake.PrivateIPs(), 1)
	assert.Len(t, fake.PublicIPs(), 1)
}

// A leaked private IP that never got a public IP attached — the crash window
// between the two creates — must still be collected.
func TestReconcileCollectsALeakedPrivateIPWithNoPublicIP(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, _ := newTestAllocator(t, fake)

	priv, err := fake.AssignPrivateIP(ctx, "ocid1.vnic.oc1..vnic1", netip.Addr{}, ocinet.DisplayNamePrefix+"eipalloc-half")
	require.NoError(t, err)

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{priv.ID}, res.Collected)
	assert.Empty(t, fake.PrivateIPs())
}

// A pool that has never allocated has no record. That is an empty binding set,
// not a failure — and the pass still has to run, since the leak it exists to
// find is an OCI object created before its binding was ever written.
func TestReconcileOnAPoolThatHasNeverAllocated(t *testing.T) {
	fake := oci.NewFake()
	alloc, store := newTestAllocator(t, fake)

	if _, err := store.Get(context.Background(), "oci-wan"); err == nil {
		t.Fatal("precondition: expected no record for a pool that never allocated")
	}

	res, err := alloc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile on an empty pool: %v", err)
	}
	if len(res.Collected) != 0 || len(res.Stale) != 0 {
		t.Errorf("expected nothing to collect: %+v", res)
	}
}

// newPeerAllocator builds a second node's allocator over the same store: one
// KV record per pool, one VNIC per node, which is the shape a multi-node
// cluster actually has.
func newPeerAllocator(t *testing.T, fake *oci.Fake, store ocinet.Store, vnicID string) *ocinet.PoolAllocator {
	t.Helper()
	a, err := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-wan", Source: external.SourceOCI},
		VNICID:        vnicID,
		CompartmentID: "ocid1.compartment.oc1..comp1",
		Schedule:      []time.Duration{time.Millisecond},
		Budget:        time.Second,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)
	return a
}

// The record is one key per pool and so cluster-wide, while the live list is
// one VNIC. A peer's binding therefore looks exactly like a binding whose
// objects OCI has lost, and dropping it makes every node delete its peers'
// addresses on startup — after which direction 1 on the owning node collects
// the now-unclaimed objects as leaks and a running instance loses its address.
func TestReconcileLeavesAnotherNodesBindingAlone(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	node1, store := newTestAllocator(t, fake)
	node2 := newPeerAllocator(t, fake, store, "ocid1.vnic.oc1..vnic2")

	ip, err := node1.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	res, err := node2.Reconcile(ctx)
	require.NoError(t, err)

	assert.Empty(t, res.Stale, "node2 dropped node1's binding")
	assert.Empty(t, res.Collected, "node2 collected node1's address")

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Contains(t, rec.Bindings, ip.String(), "node1's binding did not survive node2's reconcile")

	// The destructive half: with the binding gone, node1's own next pass would
	// see an unclaimed object carrying our prefix and delete it.
	res, err = node1.Reconcile(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Collected, "node1 collected its own live address as a leak")
	assert.Len(t, fake.PublicIPs(), 1, "the reserved public IP of a running instance was deleted")
}

// A node must still drop its own stale bindings, or an interrupted Release
// leaves an address reported to a customer as theirs forever.
func TestReconcileStillDropsItsOwnStaleBinding(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	node1, store := newTestAllocator(t, fake)
	node2 := newPeerAllocator(t, fake, store, "ocid1.vnic.oc1..vnic2")

	ip, err := node1.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	for _, p := range fake.PrivateIPs() {
		require.NoError(t, fake.UnassignPrivateIP(ctx, p.ID))
	}

	// node2 is not the owner, so it must not be the one to decide.
	res, err := node2.Reconcile(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Stale)

	res, err = node1.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Stale)
}
