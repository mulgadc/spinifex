package external

import (
	"context"
	"net/netip"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStaticAllocator(t *testing.T, pools []ExternalPoolConfig) *StaticPoolAllocator {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	a, err := NewStaticPoolAllocator(js, pools)
	require.NoError(t, err)
	return a
}

func wanPool() ExternalPoolConfig {
	return ExternalPoolConfig{
		Name:       "wan",
		RangeStart: "192.168.1.150",
		RangeEnd:   "192.168.1.160",
		Gateway:    "192.168.1.1",
		PrefixLen:  24,
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	require.NoError(t, err)
	return a
}

func TestStaticPool_AllocateSequential(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := context.Background()

	ip1, err := a.Allocate(ctx, AllocateRequest{PoolName: "wan", Purpose: "eni-public", ENIID: "eni-1", InstanceID: "i-1"})
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.151", ip1.String())

	ip2, err := a.Allocate(ctx, AllocateRequest{PoolName: "wan", Purpose: "eni-public", ENIID: "eni-2", InstanceID: "i-2"})
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.152", ip2.String())
}

func TestStaticPool_GatewayReserved(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})

	rec, err := a.GetPoolRecord("wan")
	require.NoError(t, err)
	alloc, ok := rec.Allocated["192.168.1.150"]
	require.True(t, ok)
	assert.Equal(t, purposeIGWLRP, alloc.Purpose)

	err = a.Release(context.Background(), "wan", mustAddr(t, "192.168.1.150"))
	assert.ErrorContains(t, err, "cannot release gateway IP")
}

// TestStaticPool_NoInstantReuseAfterRelease pins the round-robin reuse policy:
// a released IP must not be handed straight back (the recycle that left a
// freshly-reassigned EIP unreachable from the same host — siv-246). It is only
// reusable after the cursor cycles the range.
func TestStaticPool_NoInstantReuseAfterRelease(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := context.Background()

	ip1, err := a.Allocate(ctx, AllocateRequest{PoolName: "wan", Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)
	require.Equal(t, "192.168.1.151", ip1.String())
	_, err = a.Allocate(ctx, AllocateRequest{PoolName: "wan", Purpose: "eni-public", ENIID: "eni-2"})
	require.NoError(t, err)

	// Release the first IP; the very next allocation must NOT reuse it.
	require.NoError(t, a.Release(ctx, "wan", ip1))
	ip3, err := a.Allocate(ctx, AllocateRequest{PoolName: "wan", Purpose: "eni-public", ENIID: "eni-3"})
	require.NoError(t, err)
	assert.NotEqual(t, ip1, ip3, "released IP must not be reused immediately")
	assert.Equal(t, "192.168.1.153", ip3.String(), "allocation resumes past the cursor")

	// The released IP becomes reusable only after the cursor wraps the range.
	var reused bool
	for range 10 {
		ip, err := a.Allocate(ctx, AllocateRequest{PoolName: "wan"})
		require.NoError(t, err)
		if ip == ip1 {
			reused = true
			break
		}
	}
	assert.True(t, reused, "released IP reused only after a full cycle")
}

func TestStaticPool_Exhaustion(t *testing.T) {
	pool := ExternalPoolConfig{
		Name:       "tiny",
		RangeStart: "10.0.0.1",
		RangeEnd:   "10.0.0.3",
		Gateway:    "10.0.0.254",
		PrefixLen:  24,
	}
	a := newStaticAllocator(t, []ExternalPoolConfig{pool})
	ctx := context.Background()

	_, err := a.Allocate(ctx, AllocateRequest{PoolName: "tiny"})
	require.NoError(t, err)
	_, err = a.Allocate(ctx, AllocateRequest{PoolName: "tiny"})
	require.NoError(t, err)
	_, err = a.Allocate(ctx, AllocateRequest{PoolName: "tiny"})
	assert.ErrorContains(t, err, "InsufficientAddressCapacity")
}

func TestStaticPool_CASConflict(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]netip.Addr, 5)
	errs := make([]error, 5)
	for i := range 5 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = a.Allocate(ctx, AllocateRequest{PoolName: "wan"})
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i, err := range errs {
		require.NoError(t, err, "concurrent allocation %d should succeed", i)
		ip := results[i].String()
		assert.False(t, seen[ip], "duplicate IP %s", ip)
		seen[ip] = true
	}
}

func TestStaticPool_ReleaseUnknown(t *testing.T) {
	a := newStaticAllocator(t, []ExternalPoolConfig{wanPool()})
	err := a.Release(context.Background(), "wan", mustAddr(t, "192.168.1.200"))
	assert.ErrorContains(t, err, "not allocated")
}

// TestNextAvailableIP_SkipsGwLrpRange verifies the static allocator honors
// gw_lrp_range — addresses inside the reservation are skipped (siv-36).
func TestNextAvailableIP_SkipsGwLrpRange(t *testing.T) {
	rec := &PoolRecord{
		PoolName:        "wan",
		RangeStart:      "192.168.1.10",
		RangeEnd:        "192.168.1.14",
		GwLrpRangeStart: "192.168.1.11",
		GwLrpRangeEnd:   "192.168.1.13",
		Allocated:       map[string]ExternalIPAllocation{},
	}
	ip, err := nextAvailableIP(rec)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.10", ip)
	rec.Allocated[ip] = ExternalIPAllocation{}

	ip, err = nextAvailableIP(rec)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.14", ip)
	rec.Allocated[ip] = ExternalIPAllocation{}

	_, err = nextAvailableIP(rec)
	require.Error(t, err)
}
