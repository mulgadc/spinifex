package ocinet_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/ocinet"
)

// newTestAllocator wires a fake OCI and an in-memory store, with a poll ladder
// that does not sleep so the async-assignment tests stay fast.
func newTestAllocator(t *testing.T, fake *oci.Fake) (*ocinet.PoolAllocator, *ocinet.MemStore) {
	t.Helper()
	store := ocinet.NewMemStore()
	a, err := ocinet.New(fake, store, ocinet.Config{
		Pool:          external.ExternalPoolConfig{Name: "oci-wan", Source: external.SourceOCI},
		VNICID:        "ocid1.vnic.oc1..vnic1",
		CompartmentID: "ocid1.compartment.oc1..comp1",
		Schedule:      []time.Duration{time.Millisecond},
		Budget:        time.Second,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)
	return a, store
}

func TestAllocateCreatesThePairAndReturnsThePublicAddress(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	got, err := a.Allocate(ctx, external.AllocateRequest{
		PoolName: "oci-wan", Purpose: "eip", AllocationID: "eipalloc-1", ENIID: "eni-1",
	})
	require.NoError(t, err)

	// The public address is what AWS semantics are about, so that is what
	// Allocate returns — not the private one that rides the wire.
	assert.Equal(t, "203.0.113.10", got.String())

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	b, ok := rec.Bindings[got.String()]
	require.True(t, ok, "binding not recorded under the public address")
	assert.Equal(t, "10.200.0.100", b.PrivateAddr, "private half must be recorded for the host plumbing")
	assert.Equal(t, "eni-1", b.ENIID)
	assert.NotEmpty(t, b.PublicIPID)
	assert.NotEmpty(t, b.PrivateIPID)

	require.Len(t, fake.PrivateIPs(), 1)
	require.Len(t, fake.PublicIPs(), 1)
	assert.Equal(t, ocinet.DisplayNamePrefix+"eipalloc-1", fake.PrivateIPs()[0].DisplayName,
		"objects must carry the prefix or reconcile cannot tell them from the operator's")
}

func TestAllocateWaitsOutAsyncAssignment(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	fake.AssignAfter = 2 // ASSIGNING until the second Get
	a, _ := newTestAllocator(t, fake)

	got, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	assert.True(t, got.IsValid())

	pubs := fake.PublicIPs()
	require.Len(t, pubs, 1)
	assert.Equal(t, oci.LifecycleStateAssigned, pubs[0].LifecycleState,
		"Allocate must not return an address that is still provisioning")
}

func TestAllocateSurfacesQuotaExhaustionAsInsufficientAddressCapacity(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	fake.PrivateLimit = 1
	a, _ := newTestAllocator(t, fake)

	_, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	_, err = a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInsufficientAddressCapacity,
		"an OCI quota refusal must reach the customer as the AWS error that means the same thing")
}

// A failure after the private IP exists must not leave it behind: we are still
// running and know exactly what we just created, so rolling back beats leaving
// a billable object for the next reconcile pass.
func TestAllocateRollsBackThePrivateIPWhenThePublicIPFails(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	fake.FailWith["CreatePublicIP"] = errors.New("boom")
	a, store := newTestAllocator(t, fake)

	_, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.Error(t, err)

	assert.Empty(t, fake.PrivateIPs(), "private IP was not rolled back")
	assert.Empty(t, fake.PublicIPs())
	_, err = store.Get(ctx, "oci-wan")
	require.ErrorIs(t, err, kvstore.ErrNotFound, "a failed allocate must leave no record at all")
}

func TestReleaseDeletesBothObjectsAndDropsTheBinding(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	ip, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1", ENIID: "eni-1"})
	require.NoError(t, err)

	// Unscoped, as ReleaseAddress is: an owner-scoped release may not free an
	// EIP, which TestAnOwnerScopedReleaseWillNotFreeAnEIPPair covers.
	require.NoError(t, a.Release(ctx, "oci-wan", ip, ""))

	assert.Empty(t, fake.PrivateIPs(), "private IP outlived its release")
	assert.Empty(t, fake.PublicIPs(), "reserved public IP outlived its release — this one bills")
	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Empty(t, rec.Bindings)
}

// External addresses are recycled and teardown sweeps re-emit releases, so a
// release naming an owner that no longer holds the binding must be a no-op
// rather than a double-free of an address already handed to another instance.
func TestReleaseWithAStaleOwnerIsANoOp(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	// Auto-assigned, so the stale-owner guard is what decides this and not the
	// EIP refusal next to it.
	ip, err := a.Allocate(ctx, external.AllocateRequest{
		PoolName: "oci-wan", Purpose: "eni-public", ENIID: "eni-current", InstanceID: "i-1",
	})
	require.NoError(t, err)

	require.NoError(t, a.Release(ctx, "oci-wan", ip, "eni-previous"))

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Len(t, rec.Bindings, 1, "a stale release freed an address belonging to a live ENI")
	assert.Len(t, fake.PublicIPs(), 1)
}

func TestReleaseOfAnUnknownAddressIsANoOpWhenOwnerScoped(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestAllocator(t, oci.NewFake())

	// Owner-scoped: a duplicated teardown for an address already freed.
	require.NoError(t, a.Release(ctx, "oci-wan", netip.MustParseAddr("203.0.113.99"), "eni-1"))
	// Unscoped: nothing claims to know better, so this is a real error.
	require.Error(t, a.Release(ctx, "oci-wan", netip.MustParseAddr("203.0.113.99"), ""))
}

func TestPoolMismatchIsRefused(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestAllocator(t, oci.NewFake())

	_, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "other-pool", AllocationID: "eipalloc-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool mismatch")

	err = a.Release(ctx, "other-pool", netip.MustParseAddr("203.0.113.10"), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool mismatch")
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	store := ocinet.NewMemStore()
	fake := oci.NewFake()

	_, err := ocinet.New(nil, store, ocinet.Config{VNICID: "v", CompartmentID: "c"})
	require.Error(t, err)

	_, err = ocinet.New(fake, nil, ocinet.Config{VNICID: "v", CompartmentID: "c"})
	require.Error(t, err)

	_, err = ocinet.New(fake, store, ocinet.Config{CompartmentID: "c"})
	require.ErrorContains(t, err, "vnic_id")

	_, err = ocinet.New(fake, store, ocinet.Config{VNICID: "v"})
	require.ErrorContains(t, err, "compartment_id")
}

func TestBindingForReturnsThePrivateHalf(t *testing.T) {
	ctx := context.Background()
	a, _ := newTestAllocator(t, oci.NewFake())

	ip, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	b, ok, err := a.BindingFor(ctx, ip)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "10.200.0.100", b.PrivateAddr)

	_, ok, err = a.BindingFor(ctx, netip.MustParseAddr("203.0.113.200"))
	require.NoError(t, err)
	assert.False(t, ok)
}

// The VNIC on the binding is what lets a node returning from a failure tell an
// address that is still its own from one that has moved to a surviving node,
// before it reasserts host plumbing for an address it no longer holds.
func TestAllocateRecordsTheOwningVNIC(t *testing.T) {
	ctx := context.Background()
	a, store := newTestAllocator(t, oci.NewFake())

	ip, err := a.Allocate(ctx, external.AllocateRequest{PoolName: "oci-wan", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Equal(t, "ocid1.vnic.oc1..vnic1", rec.Bindings[ip.String()].VNICID)
}
