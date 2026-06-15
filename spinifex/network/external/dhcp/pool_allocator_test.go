package dhcp_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPoolAllocator(t *testing.T, fake *dhcp.Fake, pool external.ExternalPoolConfig) (*dhcp.DHCPPoolAllocator, *dhcp.Store) {
	t.Helper()
	mgr, store, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	client := dhcp.NewNATSClient(nc, 3*time.Second)
	return dhcp.NewDHCPPoolAllocator(client, pool), store
}

func TestDHCPPoolAllocatorAllocateAndRelease(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{
		Name:       "wan",
		Source:     external.SourceDHCP,
		BindBridge: "br-wan",
	}
	allocator, store := newPoolAllocator(t, fake, pool)

	addr, err := allocator.Allocate(context.Background(), external.AllocateRequest{
		PoolName:     "wan",
		AllocationID: "eipalloc-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.100", addr.String())

	entry, err := store.Get("eipalloc-1")
	require.NoError(t, err)
	assert.Equal(t, dhcp.PurposeEIP, entry.Purpose)
	assert.Equal(t, "wan", entry.PoolName)

	require.NoError(t, allocator.Release(context.Background(), "wan", addr, ""))
	_, err = store.Get("eipalloc-1")
	assert.Error(t, err)
	assert.Equal(t, 1, fake.ReleaseCount())
}

func TestDHCPPoolAllocatorClientIDPrecedence(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP, BindBridge: "br-wan"}
	allocator, store := newPoolAllocator(t, fake, pool)

	cases := []struct {
		req      external.AllocateRequest
		clientID string
		purpose  string
	}{
		{external.AllocateRequest{AllocationID: "eipalloc-x", ENIID: "eni-y", InstanceID: "i-z"}, "eipalloc-x", dhcp.PurposeEIP},
		{external.AllocateRequest{ENIID: "eni-y", InstanceID: "i-z"}, "eni-y", dhcp.PurposeENIPublic},
		{external.AllocateRequest{InstanceID: "i-z"}, "i-z", dhcp.PurposeENIPublic},
	}
	for _, tc := range cases {
		_, err := allocator.Allocate(context.Background(), tc.req)
		require.NoError(t, err, "case %+v", tc.req)
		entry, err := store.Get(tc.clientID)
		require.NoError(t, err, "client id %q not persisted", tc.clientID)
		assert.Equal(t, tc.purpose, entry.Purpose)
	}
}

func TestDHCPPoolAllocatorRejectsMissingIdentity(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP, BindBridge: "br-wan"}
	allocator, _ := newPoolAllocator(t, fake, pool)

	_, err := allocator.Allocate(context.Background(), external.AllocateRequest{PoolName: "wan"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AllocationID")
}

func TestDHCPPoolAllocatorRejectsStaticPool(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{Name: "wan", Source: external.SourceStatic}
	allocator, _ := newPoolAllocator(t, fake, pool)

	_, err := allocator.Allocate(context.Background(), external.AllocateRequest{PoolName: "wan", AllocationID: "eipalloc-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not dhcp-sourced")
}

func TestDHCPPoolAllocatorRejectsPoolMismatch(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP, BindBridge: "br-wan"}
	allocator, _ := newPoolAllocator(t, fake, pool)

	_, err := allocator.Allocate(context.Background(), external.AllocateRequest{PoolName: "other", AllocationID: "eipalloc-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool mismatch")
}

func TestDHCPPoolAllocatorReleaseUnknownIPIsNoop(t *testing.T) {
	fake := dhcp.NewFake()
	pool := external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP, BindBridge: "br-wan"}
	allocator, _ := newPoolAllocator(t, fake, pool)

	addr := netip.MustParseAddr("198.51.100.99")
	assert.NoError(t, allocator.Release(context.Background(), "wan", addr, ""))
	assert.Equal(t, 0, fake.ReleaseCount())
}
