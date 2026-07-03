package external

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routedTestPool() *ExternalPoolConfig {
	return &ExternalPoolConfig{
		Name: "nat-transit", Gateway: "100.127.0.1", PrefixLen: 24,
		GwLrpRangeStart: "100.127.0.240", GwLrpRangeEnd: "100.127.0.243",
	}
}

func findSNAT(m *mock.Client, logicalIP string) *nbdb.NAT {
	for _, n := range m.NATs {
		if n.Type == "snat" && n.LogicalIP == logicalIP {
			return n
		}
	}
	return nil
}

func TestAttachIGW_Routed_AllocatesTransitIPAndInstallsSNAT(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeRouted, routedTestPool(), NewStaticRangeAllocator(m), []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))

	localnet, err := m.GetLogicalSwitchPort(ctx, topology.ExternalLocalnetPortShared())
	require.NoError(t, err)
	assert.Equal(t, "router", localnet.Options["nat-addresses"], "routed mode must set nat-addresses=router")

	lrp, err := m.GetLogicalRouterPort(ctx, topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"100.127.0.240/24"}, lrp.Networks, "gateway LRP must get a transit pool IP")

	snat := findSNAT(m, "10.0.0.0/16")
	require.NotNil(t, snat, "routed mode must install snat VPC CIDR -> gateway LRP IP")
	assert.Equal(t, "100.127.0.240", snat.ExternalIP)
	assert.Equal(t, "igw-default-snat", snat.ExternalIDs["spinifex:role"])
}

func TestAttachIGW_Routed_MissingVPCCIDRFails(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeRouted, routedTestPool(), NewStaticRangeAllocator(m), []string{"chassis-a"})

	err := mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spinifex:cidr")
}

func TestDetachIGW_Routed_RemovesSNAT(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newTestIGWManager(t, m, policy.NATModeRouted, routedTestPool(), NewStaticRangeAllocator(m), []string{"chassis-a"})

	require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
	require.NotNil(t, findSNAT(m, "10.0.0.0/16"))

	require.NoError(t, mgr.DetachIGW(ctx, "vpc-1"))
	assert.Nil(t, findSNAT(m, "10.0.0.0/16"), "detach must remove the routed SNAT")
}

func TestNewManagers_AcceptRoutedMode(t *testing.T) {
	m := mock.New()
	nm, err := policy.NewNATManager(m, policy.NATModeRouted)
	require.NoError(t, err)
	_, err = NewIGWManager(IGWManagerConfig{
		OVN: m, Routes: policy.NewRouteManager(m), NAT: nm,
		Allocator: NewStaticRangeAllocator(m), NATMode: policy.NATModeRouted,
	})
	require.NoError(t, err)
}
