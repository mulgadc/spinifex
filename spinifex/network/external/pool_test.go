package external

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func TestFindPool_AZBeatsRegionBeatsUnscoped(t *testing.T) {
	pools := []ExternalPoolConfig{
		{Name: "default"},
		{Name: "region-us", Region: "us"},
		{Name: "az-us-1a", Region: "us", AZ: "us-1a"},
	}

	assert.Equal(t, "az-us-1a", FindPool(pools, "us", "us-1a").Name)
	assert.Equal(t, "region-us", FindPool(pools, "us", "us-1b").Name)
	assert.Equal(t, "default", FindPool(pools, "eu", "eu-1a").Name)
}

func TestFindPool_NoMatchReturnsNil(t *testing.T) {
	pools := []ExternalPoolConfig{{Name: "region-us", Region: "us"}}
	assert.Nil(t, FindPool(pools, "eu", "eu-1a"))
}

func TestLinkLocalAllocator_AlwaysReturnsNotOK(t *testing.T) {
	a := LinkLocalAllocator{}
	ip, prefix, nexthop, ok, err := a.Allocate(context.Background(), "vpc-1", &ExternalPoolConfig{Gateway: "192.168.1.1", PrefixLen: 24})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, ip)
	assert.Empty(t, nexthop)
	assert.Zero(t, prefix)
	require.NoError(t, a.Release(context.Background(), "vpc-1"))
}

func TestStaticRangeAllocator_ExplicitRange_AllocatesFirstFree(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	a := NewStaticRangeAllocator(m)

	pool := &ExternalPoolConfig{
		Name:            "p",
		Gateway:         "192.168.1.1",
		PrefixLen:       24,
		GwLrpRangeStart: "192.168.1.240",
		GwLrpRangeEnd:   "192.168.1.243",
	}

	ip, prefix, nexthop, ok, err := a.Allocate(ctx, "vpc-1", pool)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.240", ip)
	assert.Equal(t, 24, prefix)
	assert.Equal(t, "192.168.1.1", nexthop, "static allocator nexthop must come from pool.Gateway")
}

func TestStaticRangeAllocator_SkipsUsedIPs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-other")}))
	require.NoError(t, m.CreateLogicalRouterPort(ctx, topology.VPCRouter("vpc-other"), &nbdb.LogicalRouterPort{
		Name:        topology.GatewayRouterPort("vpc-other"),
		ExternalIDs: map[string]string{gatewayIPExtIDKey: "192.168.1.240"},
	}))
	a := NewStaticRangeAllocator(m)

	pool := &ExternalPoolConfig{
		Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.243",
	}

	ip, _, _, ok, err := a.Allocate(ctx, "vpc-new", pool)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.241", ip)
}

func TestStaticRangeAllocator_ReturnsExistingForSameVPC(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-1")}))
	require.NoError(t, m.CreateLogicalRouterPort(ctx, topology.VPCRouter("vpc-1"), &nbdb.LogicalRouterPort{
		Name:        topology.GatewayRouterPort("vpc-1"),
		ExternalIDs: map[string]string{gatewayIPExtIDKey: "192.168.1.242"},
	}))
	a := NewStaticRangeAllocator(m)

	pool := &ExternalPoolConfig{
		Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.243",
	}

	ip, _, nexthop, ok, err := a.Allocate(ctx, "vpc-1", pool)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.242", ip)
	assert.Equal(t, "192.168.1.1", nexthop)
}

func TestStaticRangeAllocator_RangeExhaustedReturnsError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-other")}))
	require.NoError(t, m.CreateLogicalRouterPort(ctx, topology.VPCRouter("vpc-other"), &nbdb.LogicalRouterPort{
		Name:        topology.GatewayRouterPort("vpc-other"),
		ExternalIDs: map[string]string{gatewayIPExtIDKey: "192.168.1.240"},
	}))
	a := NewStaticRangeAllocator(m)
	pool := &ExternalPoolConfig{
		Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.240",
	}

	_, _, _, ok, err := a.Allocate(ctx, "vpc-new", pool)
	require.Error(t, err)
	assert.False(t, ok)
}

func TestStaticRangeAllocator_NoRangeReturnsNotOK(t *testing.T) {
	a := NewStaticRangeAllocator(mock.New())
	_, _, _, ok, err := a.Allocate(context.Background(), "vpc-1", &ExternalPoolConfig{})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGwLrpRange_AutoDeriveTopOfSubnet(t *testing.T) {
	pool := &ExternalPoolConfig{Gateway: "192.168.1.1", PrefixLen: 24}
	start, end, prefix, ok := gwLrpRange(pool)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.239", start.String())
	assert.Equal(t, "192.168.1.254", end.String())
	assert.Equal(t, 24, prefix)
}

func TestGwLrpRange_ShiftsBelowEIPRangeOnOverlap(t *testing.T) {
	pool := &ExternalPoolConfig{
		Gateway:    "192.168.1.1",
		PrefixLen:  24,
		RangeStart: "192.168.1.240",
		RangeEnd:   "192.168.1.250",
	}
	start, end, _, ok := gwLrpRange(pool)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.224", start.String())
	assert.Equal(t, "192.168.1.239", end.String())
}

func TestGwLrpRange_ExplicitTakesPriority(t *testing.T) {
	pool := &ExternalPoolConfig{
		Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.10",
		GwLrpRangeEnd:   "192.168.1.20",
	}
	start, end, _, ok := gwLrpRange(pool)
	require.True(t, ok)
	assert.Equal(t, "192.168.1.10", start.String())
	assert.Equal(t, "192.168.1.20", end.String())
}

func TestGwLrpRange_NilPoolReturnsNotOK(t *testing.T) {
	_, _, _, ok := gwLrpRange(nil)
	assert.False(t, ok)
}

func TestGwLrpRange_GatewayInsideAutoRangeIsExcluded(t *testing.T) {
	// Tiny /29 forces the auto-range to overlap the gateway IP.
	pool := &ExternalPoolConfig{Gateway: "192.168.1.1", PrefixLen: 29}
	start, end, _, ok := gwLrpRange(pool)
	require.True(t, ok)
	startU := ipv4ToUint32(start)
	endU := ipv4ToUint32(end)
	gwU := ipv4ToUint32(net.ParseIP(pool.Gateway).To4())
	for n := startU; n <= endU; n++ {
		assert.NotEqual(t, gwU, n, "gateway IP must not appear in auto-range")
	}
}
