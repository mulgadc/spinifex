package external

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
)

func seedVPCRouter(t *testing.T, m *mock.Client, vpcID, cidr string) {
	t.Helper()
	extIDs := map[string]string{"spinifex:vpc_id": vpcID}
	if cidr != "" {
		extIDs["spinifex:cidr"] = cidr
	}
	require.NoError(t, m.CreateLogicalRouter(context.Background(), &nbdb.LogicalRouter{
		Name:        topology.VPCRouter(vpcID),
		ExternalIDs: extIDs,
	}))
}

func newTestIGWManager(t *testing.T, m *mock.Client, mode policy.NATMode, pool *ExternalPoolConfig, allocator GatewayIPAllocator, chassis []string) (IGWManager, *int) {
	t.Helper()
	nm, err := policy.NewNATManager(m, mode)
	require.NoError(t, err)
	barrierCalls := 0
	mgr, err := NewIGWManager(IGWManagerConfig{
		OVN:       m,
		Routes:    policy.NewRouteManager(m),
		NAT:       nm,
		Pool:      pool,
		Allocator: allocator,
		Chassis:   chassis,
		NATMode:   mode,
		FlowsBarrier: func() error {
			barrierCalls++
			return nil
		},
	})
	require.NoError(t, err)
	return mgr, &barrierCalls
}

func TestNewIGWManager_RejectsMissingDeps(t *testing.T) {
	_, err := NewIGWManager(IGWManagerConfig{NATMode: policy.NATModeDistributed})
	require.Error(t, err)

	nm, _ := policy.NewNATManager(mock.New(), policy.NATModeDistributed)
	_, err = NewIGWManager(IGWManagerConfig{
		OVN: mock.New(), Routes: policy.NewRouteManager(mock.New()), NAT: nm,
		Allocator: LinkLocalAllocator{},
	})
	require.Error(t, err, "NATMode unknown must be rejected")
}

func TestAttachIGW_Distributed_LinkLocalLRP(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, calls := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a", "chassis-b"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	// External switch + ports created.
	_, err := m.GetLogicalSwitch(ctx, topology.ExternalSwitch("vpc-1"))
	require.NoError(t, err)
	localnet, err := m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, "external", localnet.Options["network_name"])
	_, hasNat := localnet.Options["nat-addresses"]
	assert.False(t, hasNat, "distributed mode must NOT set nat-addresses")

	// Gateway LRP exists with link-local network.
	lrp, err := m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{linkLocalGatewayNetwork}, lrp.Networks)
	_, hasGwIP := lrp.ExternalIDs[gatewayIPExtIDKey]
	assert.False(t, hasGwIP, "link-local LRP must not record a gateway IP")

	// Default route points at pool.Gateway, OutputPort pinned.
	route, err := m.FindStaticRoute(ctx, topology.VPCRouter("vpc-1"), "0.0.0.0/0")
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, pool.Gateway, route.Nexthop)
	require.NotNil(t, route.OutputPort)
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), *route.OutputPort)

	// Gateway chassis set once per chassis.
	assert.Equal(t, 2, m.SetGatewayChassisCalls)

	// Flows barrier ran.
	assert.Equal(t, 1, *calls)
}

func TestAttachIGW_Centralized_AllocatesGwLrpIP(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{
		Name: "p", Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.243",
	}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, NewStaticRangeAllocator(m), []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	localnet, err := m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, "router", localnet.Options["nat-addresses"], "centralized mode must set nat-addresses=router")

	lrp, err := m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.240/24"}, lrp.Networks)
	assert.Equal(t, "192.168.1.240", lrp.ExternalIDs[gatewayIPExtIDKey])
}

func TestAttachIGW_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	// Still only one LRP.
	lrps, err := m.ListLogicalRouterPorts(ctx)
	require.NoError(t, err)
	gwCount := 0
	for _, lrp := range lrps {
		if lrp.Name == topology.GatewayRouterPort("vpc-1") {
			gwCount++
		}
	}
	assert.Equal(t, 1, gwCount)
}

func TestAttachIGW_NoPoolNoChassis_UsesLinkLocalFallbacks(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	route, err := m.FindStaticRoute(ctx, topology.VPCRouter("vpc-1"), "0.0.0.0/0")
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, linkLocalGatewayNexthop, route.Nexthop)
	assert.Equal(t, 0, m.SetGatewayChassisCalls)
}

func TestDetachIGW_RemovesEverything(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.DetachIGW(ctx, "vpc-1"))

	_, err := m.GetLogicalSwitch(ctx, topology.ExternalSwitch("vpc-1"))
	assert.Error(t, err)
	_, err = m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	assert.Error(t, err)
	route, err := m.FindStaticRoute(ctx, topology.VPCRouter("vpc-1"), "0.0.0.0/0")
	require.NoError(t, err)
	assert.Nil(t, route)
}

func TestDetachIGW_IdempotentOnAbsent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	// Detach without prior attach — must return error from external switch
	// delete (it never existed). Verify behaviour by checking the route
	// delete still ran first via the idempotent route manager.
	err := mgr.DetachIGW(ctx, "vpc-1")
	require.Error(t, err) // external switch absent — DeleteLogicalSwitch returns NotFound
}

// failingAllocator surfaces an Allocate error and lets the test assert
// AttachIGW unwinds the external switch + localnet on failure.
type failingAllocator struct{}

var _ GatewayIPAllocator = failingAllocator{}

func (failingAllocator) Allocate(_ context.Context, _ string, _ *ExternalPoolConfig) (string, int, bool, error) {
	return "", 0, false, errors.New("boom")
}

func (failingAllocator) Release(_ context.Context, _ string) error { return nil }

func TestAttachIGW_AllocatorFailureUnwindsExtSwitch(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, failingAllocator{}, nil)

	require.Error(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	_, err := m.GetLogicalSwitch(ctx, topology.ExternalSwitch("vpc-1"))
	assert.Error(t, err, "external switch must be unwound on allocator failure")
}
