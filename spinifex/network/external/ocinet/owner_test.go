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

// allocateBare is an EIP as AWS makes one: an address with no interface behind
// it yet. Everything about the owner has to arrive later.
func allocateBare(t *testing.T, a *ocinet.PoolAllocator) netip.Addr {
	t.Helper()
	ip, err := a.Allocate(context.Background(), external.AllocateRequest{
		PoolName: "oci-wan", Purpose: "eip", AllocationID: "eipalloc-1",
	})
	require.NoError(t, err)
	return ip
}

func ownerOf(t *testing.T, store *ocinet.MemStore, ip netip.Addr) string {
	t.Helper()
	rec, err := store.Get(context.Background(), "oci-wan")
	require.NoError(t, err)
	return rec.Bindings[ip.String()].ENIID
}

// Without this the binding of every EIP names no ENI, and the affinity pass
// cannot tell which node the address has to be delivered to.
func TestBindOwnerRecordsTheENIAnEIPWasAssociatedWith(t *testing.T) {
	ctx := context.Background()
	a, store := newTestAllocator(t, oci.NewFake())
	ip := allocateBare(t, a)
	assert.Empty(t, ownerOf(t, store, ip), "a bare allocation should name no owner")

	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, "eni-1"))
	assert.Equal(t, "eni-1", ownerOf(t, store, ip))
}

// A disassociated address is still allocated and still billing, but no guest
// answers on it, so no node should claim it.
func TestBindOwnerClearsTheOwnerOnDisassociate(t *testing.T) {
	ctx := context.Background()
	a, store := newTestAllocator(t, oci.NewFake())
	ip := allocateBare(t, a)

	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, "eni-1"))
	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, ""))
	assert.Empty(t, ownerOf(t, store, ip))
}

// A re-association to the same ENI is the steady state of every reconcile, so
// it must not churn the record.
func TestBindOwnerDoesNotWriteWhenTheOwnerIsUnchanged(t *testing.T) {
	ctx := context.Background()
	a, store := newTestAllocator(t, oci.NewFake())
	ip := allocateBare(t, a)
	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, "eni-1"))

	before := store.Writes()
	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, "eni-1"))
	assert.Equal(t, before, store.Writes(), "an unchanged owner cost a KV write")
}

// An address this pool never handed out is not ours to annotate — inventing a
// binding for it would have Reconcile collect an address belonging to nobody.
func TestBindOwnerIgnoresAnAddressThisPoolNeverAllocated(t *testing.T) {
	ctx := context.Background()
	a, store := newTestAllocator(t, oci.NewFake())
	allocateBare(t, a)

	stranger := netip.MustParseAddr("198.51.100.7")
	require.NoError(t, a.BindOwner(ctx, "oci-wan", stranger, "eni-9"))

	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.NotContains(t, rec.Bindings, stranger.String())
}

func TestBindOwnerRefusesAnotherPoolsName(t *testing.T) {
	a, _ := newTestAllocator(t, oci.NewFake())
	ip := allocateBare(t, a)

	err := a.BindOwner(context.Background(), "some-other-pool", ip, "eni-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool mismatch")
}

// The whole point of recording the owner: an EIP associated to a guest running
// here is then claimable, which it never was before.
func TestAnEIPBecomesClaimableOnceItsOwnerIsRecorded(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newAffinityAllocator(t, fake, "port-eni-1")
	pubAddr, privID := seedForeignBinding(t, fake, store, "")

	res, err := a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	require.Empty(t, res.Claimed, "an unassociated address has no owning node")

	require.NoError(t, a.BindOwner(ctx, "oci-wan", netip.MustParseAddr(pubAddr), "eni-1"))

	res, err = a.ClaimLocalAddresses(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{pubAddr}, res.Claimed)

	got, err := fake.GetPrivateIP(ctx, privID)
	require.NoError(t, err)
	assert.Equal(t, thisVNIC, got.VNICID, "OCI still delivers the EIP to another node")
}

// On OCI the address is a reserved public IP object, so an owner-scoped release
// of one does not merely return it to a pool — it destroys it at the provider
// and the customer cannot get it back.
func TestAnOwnerScopedReleaseWillNotFreeAnEIPPair(t *testing.T) {
	ctx := context.Background()
	fake := oci.NewFake()
	a, store := newTestAllocator(t, fake)

	ip, err := a.Allocate(ctx, external.AllocateRequest{
		PoolName: "oci-wan", Purpose: "eip", AllocationID: "eipalloc-1",
	})
	require.NoError(t, err)
	require.NoError(t, a.BindOwner(ctx, "oci-wan", ip, "eni-1"))

	// What instance teardown does once the EIP is what the instance holds.
	require.NoError(t, a.Release(ctx, "oci-wan", ip, "eni-1"))

	assert.Len(t, fake.PublicIPs(), 1,
		"terminating an instance destroyed a customer's reserved public IP")
	rec, err := store.Get(ctx, "oci-wan")
	require.NoError(t, err)
	assert.Len(t, rec.Bindings, 1)
}
