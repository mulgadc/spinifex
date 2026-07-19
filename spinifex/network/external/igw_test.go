package external

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
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
	mgr, calls, err := newTestIGWManagerWithSeed(t, m, mode, pool, allocator, chassis, nil)
	require.NoError(t, err)
	return mgr, calls
}

// nexthopSeedCall records a single NexthopSeed invocation's arguments.
type nexthopSeedCall struct {
	lrpName   string
	nexthopIP string
}

// newTestIGWManagerWithSeed is newTestIGWManager plus an optional capturing
// NexthopSeed hook; pass nil to leave it unset (mirrors newTestIGWManager).
func newTestIGWManagerWithSeed(t *testing.T, m *mock.Client, mode policy.NATMode, pool *ExternalPoolConfig, allocator GatewayIPAllocator, chassis []string, seedCalls *[]nexthopSeedCall) (IGWManager, *int, error) {
	t.Helper()
	nm, err := policy.NewNATManager(m, mode)
	require.NoError(t, err)
	barrierCalls := 0
	cfg := IGWManagerConfig{
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
	}
	if seedCalls != nil {
		cfg.NexthopSeed = func(_ context.Context, lrpName, nexthopIP string) error {
			*seedCalls = append(*seedCalls, nexthopSeedCall{lrpName: lrpName, nexthopIP: nexthopIP})
			return nil
		}
	}
	mgr, err := NewIGWManager(cfg)
	return mgr, &barrierCalls, err
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

	// Shared external switch + localnet created.
	_, err := m.GetLogicalSwitch(ctx, topology.ExternalSwitchShared())
	require.NoError(t, err)
	localnet, err := m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPortShared())
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

	// lr_in_ip_routing runs before lr_in_policy: without a default static route
	// every external destination drops before the reroute policy fires.
	// AttachIGW must install 0.0.0.0/0 → pool.Gateway on the VPC router.
	route, err := m.FindStaticRoute(ctx, topology.VPCRouter("vpc-1"), "0.0.0.0/0")
	require.NoError(t, err)
	require.NotNil(t, route, "AttachIGW must install the default static route so routing forwards to gw-port")
	require.NotNil(t, route.OutputPort)
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), *route.OutputPort)
	assert.Equal(t, pool.Gateway, route.Nexthop)

	// EnsureSubnetEgress installs per-subnet policy with the expected nexthop.
	require.NoError(t, mgr.EnsureSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0")))
	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, pool.Gateway, *policies[0].Nexthop)
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), policies[0].ExternalIDs["spinifex:output_port"])
	assert.Contains(t, policies[0].Match, topology.SubnetRouterPort("subnet-pub"))

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

	localnet, err := m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPortShared())
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
	require.NotNil(t, route, "AttachIGW must install the default static route so routing forwards to gw-port")
	require.NotNil(t, route.OutputPort)
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), *route.OutputPort)
	assert.Equal(t, linkLocalGatewayNexthop, route.Nexthop)

	require.NoError(t, mgr.EnsureSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0")))
	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, linkLocalGatewayNexthop, *policies[0].Nexthop)
	assert.Equal(t, 0, m.SetGatewayChassisCalls)
}

func TestDetachIGW_RemovesVPCAttachmentPreservesSharedSwitch(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.DetachIGW(ctx, "vpc-1"))

	// Per-VPC attachment (gateway switch port + LRP + default route) is gone.
	_, err := m.GetLogicalSwitchPort(ctx, topology.GatewaySwitchPort("vpc-1"))
	assert.Error(t, err)
	_, err = m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	assert.Error(t, err)
	route, err := m.FindStaticRoute(ctx, topology.VPCRouter("vpc-1"), "0.0.0.0/0")
	require.NoError(t, err)
	assert.Nil(t, route)

	// The shared external switch + localnet survive for other VPCs.
	_, err = m.GetLogicalSwitch(ctx, topology.ExternalSwitchShared())
	assert.NoError(t, err, "shared external switch must persist after detach")
	_, err = m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPortShared())
	assert.NoError(t, err, "shared localnet must persist after detach")
}

func TestDetachIGW_IdempotentOnAbsent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	// Detaching a VPC that was never attached is a no-op, not an error.
	require.NoError(t, mgr.DetachIGW(ctx, "vpc-1"))
}

type failingAllocator struct{}

var _ GatewayIPAllocator = failingAllocator{}

func (failingAllocator) Allocate(_ context.Context, _ string, _ *ExternalPoolConfig) (string, int, string, bool, error) {
	return "", 0, "", false, errors.New("boom")
}

func (failingAllocator) Release(_ context.Context, _ string) error { return nil }

// fixedNexthopAllocator returns a non-empty nexthop so the IGW manager's
// allocator-nexthop override is exercised.
type fixedNexthopAllocator struct {
	ip      string
	prefix  int
	nexthop string
}

var _ GatewayIPAllocator = fixedNexthopAllocator{}

func (a fixedNexthopAllocator) Allocate(_ context.Context, _ string, _ *ExternalPoolConfig) (string, int, string, bool, error) {
	return a.ip, a.prefix, a.nexthop, true, nil
}

func (fixedNexthopAllocator) Release(_ context.Context, _ string) error { return nil }

// TestAttachIGW_Centralized_AllocatorNexthopOverridesPool covers the bug
// where DHCP-source pools have empty pool.Gateway and fell back to
// 169.254.0.2. Allocator-returned nexthop must win.
func TestAttachIGW_Centralized_AllocatorNexthopOverridesPool(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	// pool.Gateway empty (mirrors DHCP-source config).
	pool := &ExternalPoolConfig{Name: "p", PrefixLen: 24}
	alloc := fixedNexthopAllocator{ip: "192.168.1.50", prefix: 24, nexthop: "192.168.1.254"}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, alloc, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	require.NoError(t, mgr.EnsureSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0")))
	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, "192.168.1.254", *policies[0].Nexthop, "allocator-supplied nexthop must override pool/link-local fallback")
}

func TestAttachIGW_AllocatorFailureLeavesNoGatewayPort(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, failingAllocator{}, nil)

	require.Error(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	// Allocation fails before any per-VPC object is created: no gateway LRP and
	// no gateway switch port. The shared switch is not VPC-owned, so it may exist.
	_, err := m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	assert.Error(t, err, "gateway LRP must not be created when allocation fails")
	_, err = m.GetLogicalSwitchPort(ctx, topology.GatewaySwitchPort("vpc-1"))
	assert.Error(t, err, "gateway switch port must not be created when allocation fails")
}

// TestEnsureNATGatewaySubnetEgress installs an LR policy at NATGW priority
// (lower than IGW) reusing the gateway port. Without it, SNAT rewrites the
// source but the packet has no route out of the LR.
func TestEnsureNATGatewaySubnetEgress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, policy.SubnetEgressPriorityNATGW, policies[0].Priority)
	require.NotNil(t, policies[0].Nexthop)
	assert.Equal(t, pool.Gateway, *policies[0].Nexthop)
	assert.Equal(t, topology.GatewayRouterPort("vpc-1"), policies[0].ExternalIDs["spinifex:output_port"])
	assert.Contains(t, policies[0].Match, topology.SubnetRouterPort("subnet-priv"))
}

// TestEnsureNATGatewaySubnetEgress_IsIdempotent re-runs the install and
// asserts the row count stays at 1 (no drift, no duplicate).
func TestEnsureNATGatewaySubnetEgress_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
	require.NoError(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 1)
}

// TestRemoveNATGatewaySubnetEgress removes the policy installed by
// EnsureNATGatewaySubnetEgress and is idempotent when absent.
func TestRemoveNATGatewaySubnetEgress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
	require.NoError(t, mgr.RemoveNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)

	// Idempotent: second remove is a no-op.
	require.NoError(t, mgr.RemoveNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
}

// TestNATGatewaySubnetEgress_RejectsEmptyArgs guards the helper validation
// for callers that hand in zero values from malformed events.
func TestNATGatewaySubnetEgress_RejectsEmptyArgs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	require.Error(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
	require.Error(t, mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "", netip.MustParsePrefix("0.0.0.0/0")))
	require.Error(t, mgr.RemoveNATGatewaySubnetEgress(ctx, "", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
	require.Error(t, mgr.RemoveNATGatewaySubnetEgress(ctx, "vpc-1", "", netip.MustParsePrefix("0.0.0.0/0")))
}

// TestEnsureNATGatewaySubnetEgress_RequiresAttachedIGW asserts a clear error
// when AttachIGW hasn't run — the gateway nexthop must exist or we'd install
// an unreachable policy. NATGW depends on IGW per AWS.
func TestEnsureNATGatewaySubnetEgress_RequiresAttachedIGW(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", PrefixLen: 24} // no Gateway → distributed link-local
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, failingAllocator{}, nil)

	err := mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0"))
	require.Error(t, err, "missing gateway nexthop must surface as an error")
}

// TestRemoveNATGatewaySubnetEgress_PropagatesDeleteError verifies that an OVN
// delete error (e.g. router missing) bubbles up; DeleteSubnetEgress targets the
// VPC router by name and the mock returns "router not found".
func TestRemoveNATGatewaySubnetEgress_PropagatesDeleteError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	err := mgr.RemoveNATGatewaySubnetEgress(ctx, "vpc-missing", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0"))
	require.Error(t, err)
}

// TestEnsureSystemInstanceEgress installs the /32 reroute plus the snat-only
// NAT, and asserts no inbound dnat_and_snat row exists (egress-only).
func TestEnsureSystemInstanceEgress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	require.Len(t, policies, 1)
	assert.Equal(t, policy.SystemInstanceEgressPriority, policies[0].Priority)
	assert.Contains(t, policies[0].Match, "ip4.src == 10.0.4.10/32")

	var snat, dnat *nbdb.NAT
	for _, n := range m.NATs {
		switch n.Type {
		case "snat":
			snat = n
		case "dnat_and_snat":
			dnat = n
		}
	}
	require.NotNil(t, snat, "egress snat must be installed")
	assert.Equal(t, "203.0.113.7", snat.ExternalIP)
	assert.Equal(t, "10.0.4.10/32", snat.LogicalIP)
	assert.Nil(t, dnat, "egress-only: no inbound dnat_and_snat may be installed")
}

// TestEnsureEIPInstanceEgress installs the /32 reroute above the drop gate WITHOUT
// any snat row: an EIP's dnat_and_snat already SNATs the instance, so a second plain
// snat would be redundant. The reroute alone lets the EIP's inbound-connection reply
// bypass the subnet drop gate.
func TestEnsureEIPInstanceEgress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	// Drop gate first (1100), then the EIP reroute above it (1200).
	require.NoError(t, mgr.EnsureSubnetEgressDrop(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0")))
	require.NoError(t, mgr.EnsureEIPInstanceEgress(ctx, "vpc-1", "subnet-pub", "10.0.4.10"))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	var reroute, drop *nbdb.LogicalRouterPolicy
	for i := range policies {
		switch policies[i].Priority {
		case policy.SystemInstanceEgressPriority:
			reroute = &policies[i]
		case policy.SubnetEgressPriorityDrop:
			drop = &policies[i]
		}
	}
	require.NotNil(t, reroute, "EIP /32 reroute must be installed")
	require.NotNil(t, drop, "drop gate must remain — the reroute exempts the EIP, not the subnet")
	assert.Equal(t, "reroute", reroute.Action)
	assert.Contains(t, reroute.Match, "ip4.src == 10.0.4.10/32")
	assert.Greater(t, reroute.Priority, drop.Priority, "EIP reroute must sit above the drop gate")

	for _, n := range m.NATs {
		assert.NotEqual(t, "snat", n.Type, "EIP egress is reroute-only: dnat_and_snat already SNATs, no plain snat")
	}
}

// TestEnsureSystemInstanceEgress_IsIdempotent re-runs install; row counts stay 1.
func TestEnsureSystemInstanceEgress_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))
	require.NoError(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Len(t, policies, 1)
	snatCount := 0
	for _, n := range m.NATs {
		if n.Type == "snat" {
			snatCount++
		}
	}
	assert.Equal(t, 1, snatCount)
}

// TestRemoveSystemInstanceEgress tears down both the reroute and the snat and
// is idempotent on a second call.
func TestRemoveSystemInstanceEgress(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, nil)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NoError(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))
	require.NoError(t, mgr.RemoveSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
	require.NoError(t, err)
	assert.Empty(t, policies)
	for _, n := range m.NATs {
		assert.NotEqual(t, "snat", n.Type, "snat must be removed")
	}

	require.NoError(t, mgr.RemoveSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7"))
}

// TestSystemInstanceEgress_RejectsBadArgs guards validation for malformed events.
func TestSystemInstanceEgress_RejectsBadArgs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, nil)
	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	require.Error(t, mgr.EnsureSystemInstanceEgress(ctx, "", "subnet-k3s", "10.0.4.10", "203.0.113.7"))
	require.Error(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "", "10.0.4.10", "203.0.113.7"))
	require.Error(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "not-an-ip", "203.0.113.7"))
	require.Error(t, mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", ""))
}

// TestEnsureSystemInstanceEgress_RequiresAttachedIGW asserts a clear error when
// no gateway nexthop exists — without it the reroute would be unreachable.
func TestEnsureSystemInstanceEgress_RequiresAttachedIGW(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", PrefixLen: 24} // no Gateway → no centralized nexthop
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, failingAllocator{}, nil)

	err := mgr.EnsureSystemInstanceEgress(ctx, "vpc-1", "subnet-k3s", "10.0.4.10", "203.0.113.7")
	require.Error(t, err)
}

// TestRemoveSubnetEgress_RejectsEmptyArgs covers the IGW-priority sibling of
// the NATGW empty-arg guard. Both vpcID and subnetID are mandatory.
func TestRemoveSubnetEgress_RejectsEmptyArgs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	require.Error(t, mgr.RemoveSubnetEgress(ctx, "", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0")))
	require.Error(t, mgr.RemoveSubnetEgress(ctx, "vpc-1", "", netip.MustParsePrefix("0.0.0.0/0")))
}

// TestRemoveSubnetEgress_PropagatesDeleteError exercises the delegate path
// to policy.RouteManager.DeleteSubnetEgress when the router doesn't exist.
func TestRemoveSubnetEgress_PropagatesDeleteError(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, nil, LinkLocalAllocator{}, nil)

	err := mgr.RemoveSubnetEgress(ctx, "vpc-missing", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0"))
	require.Error(t, err)
}

// TestAttachIGW_NexthopSeed_CalledWhenGatewayOwnsNAT covers centralized and
// routed modes, both of which give the gateway LRP a real pool IP and a
// non-empty wanNexthop — AttachIGW must invoke NexthopSeed with the gateway
// port name and the resolved nexthop right after AddDefaultRoute.
func TestAttachIGW_NexthopSeed_CalledWhenGatewayOwnsNAT(t *testing.T) {
	for _, mode := range []policy.NATMode{policy.NATModeCentralized, policy.NATModeRouted} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx := context.Background()
			m := mock.New()
			seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
			pool := &ExternalPoolConfig{
				Name: "p", Gateway: "192.168.1.1", PrefixLen: 24,
				GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.243",
			}
			var seedCalls []nexthopSeedCall
			mgr, _, err := newTestIGWManagerWithSeed(t, m, mode, pool, NewStaticRangeAllocator(m), []string{"chassis-a"}, &seedCalls)
			require.NoError(t, err)

			require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

			require.Len(t, seedCalls, 1, "NexthopSeed must be invoked exactly once on attach")
			assert.Equal(t, topology.GatewayRouterPort("vpc-1"), seedCalls[0].lrpName)
			assert.Equal(t, "192.168.1.1", seedCalls[0].nexthopIP)
		})
	}
}

// TestAttachIGW_NexthopSeed_NotCalledWhenModeIsDistributed guards the
// gatewayOwnsNAT() gate: distributed mode never owns NAT on the gateway
// chassis, so seeding a static binding there would be meaningless.
func TestAttachIGW_NexthopSeed_NotCalledWhenModeIsDistributed(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
	var seedCalls []nexthopSeedCall
	mgr, _, err := newTestIGWManagerWithSeed(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"}, &seedCalls)
	require.NoError(t, err)

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	assert.Empty(t, seedCalls, "NexthopSeed must not be invoked outside gatewayOwnsNAT modes")
}

// TestAttachIGW_NexthopSeed_NilHookSkipped asserts a nil NexthopSeed (the
// default in all existing tests/wiring paths) is simply never called, and
// AttachIGW proceeds normally.
func TestAttachIGW_NexthopSeed_NilHookSkipped(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	pool := &ExternalPoolConfig{
		Name: "p", Gateway: "192.168.1.1", PrefixLen: 24,
		GwLrpRangeStart: "192.168.1.240", GwLrpRangeEnd: "192.168.1.243",
	}
	mgr, _ := newTestIGWManager(t, m, policy.NATModeCentralized, pool, NewStaticRangeAllocator(m), []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
}
