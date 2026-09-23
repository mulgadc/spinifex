package ocinet_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
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

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Empty(t, rec.Bindings)
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
