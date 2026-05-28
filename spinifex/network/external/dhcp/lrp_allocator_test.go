package dhcp_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLRPAllocator(t *testing.T, fake *dhcp.Fake) (*dhcp.DHCPGatewayLRPAllocator, *dhcp.Manager, *dhcp.Store) {
	t.Helper()
	mgr, store, _ := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	return dhcp.NewDHCPGatewayLRPAllocator(mgr), mgr, store
}

func TestDHCPGatewayLRPAllocatorAllocate(t *testing.T) {
	fake := dhcp.NewFake()
	allocator, _, store := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{
		Name:       "wan",
		Source:     external.SourceDHCP,
		BindBridge: "br-wan",
	}
	ip, prefix, ok, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "192.0.2.100", ip)
	assert.Equal(t, 24, prefix)

	entry, err := store.Get(dhcp.GatewayLRPClientID("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, dhcp.PurposeGatewayLRP, entry.Purpose)
	assert.Equal(t, "vpc-1", entry.VPCID)
}

func TestDHCPGatewayLRPAllocatorIdempotent(t *testing.T) {
	fake := dhcp.NewFake()
	allocator, _, _ := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{
		Name:       "wan",
		Source:     external.SourceDHCP,
		BindBridge: "br-wan",
	}
	first, _, _, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	second, _, _, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, fake.AcquireCount(), "second allocate must hit the persisted lease, not DORA again")
}

func TestDHCPGatewayLRPAllocatorRelease(t *testing.T) {
	fake := dhcp.NewFake()
	allocator, _, store := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{
		Name:       "wan",
		Source:     external.SourceDHCP,
		BindBridge: "br-wan",
	}
	_, _, ok, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, allocator.Release(context.Background(), "vpc-1"))
	_, err = store.Get(dhcp.GatewayLRPClientID("vpc-1"))
	assert.Error(t, err)
	assert.Equal(t, 1, fake.ReleaseCount())
}

func TestDHCPGatewayLRPAllocatorSkipsNonDHCPPool(t *testing.T) {
	fake := dhcp.NewFake()
	allocator, _, _ := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{Name: "wan", Source: external.SourceStatic}
	ip, _, ok, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, ip)
	assert.Equal(t, 0, fake.AcquireCount())
}

func TestDHCPGatewayLRPAllocatorRequiresBindBridge(t *testing.T) {
	fake := dhcp.NewFake()
	allocator, _, _ := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP}
	_, _, ok, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "bind_bridge")
}

func TestDHCPGatewayLRPAllocatorPrefixFromMask(t *testing.T) {
	fake := dhcp.NewFake()
	fake.SetDefaultLease(dhcp.LeaseTemplate{
		IP:            net.IPv4(10, 0, 0, 50),
		SubnetMask:    net.CIDRMask(16, 32),
		ServerID:      net.IPv4(10, 0, 0, 1),
		LeaseDuration: time.Hour,
	})
	allocator, _, _ := newLRPAllocator(t, fake)

	pool := &external.ExternalPoolConfig{Name: "wan", Source: external.SourceDHCP, BindBridge: "br-wan", PrefixLen: 24}
	_, prefix, _, err := allocator.Allocate(context.Background(), "vpc-1", pool)
	require.NoError(t, err)
	assert.Equal(t, 16, prefix, "prefix should come from lease subnet mask, not pool config")
}
