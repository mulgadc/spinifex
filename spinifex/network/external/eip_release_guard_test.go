//test:in-package — reads PoolRecord.Allocated and the unexported wanPool /
// newStaticAllocator fixtures from static_pool_test.go to assert on the stored
// allocation rather than on a return value.

package external

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An EIP is allocated with no ENI and associated later, so the ownership guard
// — which only fires when the stored allocation already names an ENI — cannot
// protect it. The purpose has to.
func TestAnOwnerScopedReleaseWillNotFreeAnEIP(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := t.Context()

	eip, err := a.Allocate(ctx, AllocateRequest{
		PoolName: "wan", Purpose: "eip", AllocationID: "eipalloc-1",
	})
	require.NoError(t, err)

	// What instance teardown does: release the address the instance is holding,
	// scoped to the ENI it is holding it on. After an associate, that address is
	// the EIP.
	require.NoError(t, a.Release(ctx, "wan", eip, "eni-1"))

	rec, err := a.GetPoolRecord(ctx, "wan")
	require.NoError(t, err)
	alloc, stillHeld := rec.Allocated[eip.String()]
	require.True(t, stillHeld,
		"terminating an instance gave a customer's Elastic IP back to the pool")
	assert.Equal(t, "eipalloc-1", alloc.AllocationID)
}

// ReleaseAddress is the only caller allowed to free an EIP, and it names no
// owner precisely because there is no interface to scope to.
func TestAnUnscopedReleaseStillFreesAnEIP(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := t.Context()

	eip, err := a.Allocate(ctx, AllocateRequest{
		PoolName: "wan", Purpose: "eip", AllocationID: "eipalloc-1",
	})
	require.NoError(t, err)

	require.NoError(t, a.Release(ctx, "wan", eip, ""))

	rec, err := a.GetPoolRecord(ctx, "wan")
	require.NoError(t, err)
	_, stillHeld := rec.Allocated[eip.String()]
	assert.False(t, stillHeld, "ReleaseAddress could not free the address it owns")
}

// The guard must not reach the auto-assigned address, which is exactly what
// instance teardown is supposed to reclaim.
func TestAnOwnerScopedReleaseStillFreesAnAutoAssignedAddress(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := t.Context()

	ip, err := a.Allocate(ctx, AllocateRequest{
		PoolName: "wan", Purpose: "eni-public", ENIID: "eni-1", InstanceID: "i-1",
	})
	require.NoError(t, err)

	require.NoError(t, a.Release(ctx, "wan", ip, "eni-1"))

	rec, err := a.GetPoolRecord(ctx, "wan")
	require.NoError(t, err)
	_, stillHeld := rec.Allocated[ip.String()]
	assert.False(t, stillHeld, "an auto-assigned address leaked on teardown")
}
