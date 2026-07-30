package vpcd

import (
	"context"
	"net"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHookIGWManager(t *testing.T, m *mock.Client) external.IGWManager {
	t.Helper()
	nat, err := policy.NewNATManager(m, policy.NATModeCentralized)
	require.NoError(t, err)
	mgr, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:       m,
		Routes:    policy.NewRouteManager(m),
		NAT:       nat,
		Pool:      &external.ExternalPoolConfig{Name: "wan", PrefixLen: 23},
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeCentralized,
	})
	require.NoError(t, err)
	return mgr
}

func gwLeaseEntry(purpose, vpcID, clientID, ip string) dhcp.Entry {
	return dhcp.Entry{
		Purpose: purpose,
		VPCID:   vpcID,
		Lease: &dhcp.Lease{
			ClientID:   clientID,
			IP:         net.ParseIP(ip),
			SubnetMask: net.CIDRMask(23, 32),
		},
	}
}

func TestLeaseIPChangeHookRebindsGatewayLRP(t *testing.T) {
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(context.Background(), &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1", "spinifex:cidr": "172.31.0.0/16"},
	}))
	require.NoError(t, m.CreateLogicalRouterPort(context.Background(), topology.VPCRouter("vpc-1"), &nbdb.LogicalRouterPort{
		Name:     topology.GatewayRouterPort("vpc-1"),
		Networks: []string{"192.168.1.115/23"},
	}))

	hook := leaseIPChangeHook(newHookIGWManager(t, m))
	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "vpc-1", "dhcp-gw-lrp-vpc-1", "192.168.1.146")
	require.NoError(t, hook(context.Background(), entry, net.ParseIP("192.168.1.115")))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.146/23"}, lrp.Networks)
}

// EIP, ENI-public and NATGW addresses are API-visible resources recorded outside
// vpcd, so the hook must report the divergence rather than silently repoint a
// datapath while DescribeAddresses still returns the old IP.
func TestLeaseIPChangeHookReportsUnhandledPurposes(t *testing.T) {
	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()))

	for _, purpose := range []string{dhcp.PurposeEIP, dhcp.PurposeENIPublic, dhcp.PurposeNATGWExternal} {
		entry := gwLeaseEntry(purpose, "", "eipalloc-1", "192.168.1.146")
		err := hook(context.Background(), entry, net.ParseIP("192.168.1.115"))
		require.Error(t, err, "purpose %q must not be silently accepted", purpose)
		assert.Contains(t, err.Error(), "192.168.1.115")
		assert.Contains(t, err.Error(), "192.168.1.146")
	}
}

func TestLeaseIPChangeHookRejectsGatewayLeaseWithoutVPC(t *testing.T) {
	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()))

	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "", "dhcp-gw-lrp-vpc-1", "192.168.1.146")
	err := hook(context.Background(), entry, net.ParseIP("192.168.1.115"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no vpc_id")
}
