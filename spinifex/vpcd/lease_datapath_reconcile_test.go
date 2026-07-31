package vpcd

import (
	"context"
	"net"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedGatewayLease puts a gw-lrp lease in a real store and returns the manager
// holding it, so the reconcile runs against the same surface production uses.
func seedGatewayLease(t *testing.T, vpcID, ip string) *dhcp.Manager {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store, err := dhcp.NewStore(t.Context(), js, "az1")
	require.NoError(t, err)
	require.NoError(t, store.Put(t.Context(), dhcp.Entry{
		Purpose: dhcp.PurposeGatewayLRP,
		VPCID:   vpcID,
		Lease: &dhcp.Lease{
			ClientID:   dhcp.GatewayLRPClientID(vpcID),
			IP:         net.ParseIP(ip),
			SubnetMask: net.CIDRMask(23, 32),
		},
	}))
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{Client: dhcp.NewFake(), Store: store})
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)
	return mgr
}

func gatewayRouterWithPort(t *testing.T, vpcID, network string) *mock.Client {
	t.Helper()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(context.Background(), &nbdb.LogicalRouter{
		Name:        topology.VPCRouter(vpcID),
		ExternalIDs: map[string]string{"spinifex:vpc_id": vpcID, "spinifex:cidr": "172.31.0.0/16"},
	}))
	require.NoError(t, m.CreateLogicalRouterPort(context.Background(), topology.VPCRouter(vpcID), &nbdb.LogicalRouterPort{
		Name:     topology.GatewayRouterPort(vpcID),
		Networks: []string{network},
	}))
	return m
}

// Flipping a pool from static to DHCP leaves the port on its static address
// while the new lease goes unused, so both addresses are lost with no lease
// event to correct it.
func TestReconcileGatewayLeasesMovesDriftedPort(t *testing.T) {
	m := gatewayRouterWithPort(t, "vpc-1", "192.168.1.240/23")
	mgr := seedGatewayLease(t, "vpc-1", "192.168.1.150")

	reconcileGatewayLeases(context.Background(), mgr, newHookIGWManager(t, m))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.150/23"}, lrp.Networks)
	assert.Equal(t, "192.168.1.150", lrp.ExternalIDs["spinifex:gateway_ip"])
}

func TestReconcileGatewayLeasesLeavesMatchingPort(t *testing.T) {
	m := gatewayRouterWithPort(t, "vpc-1", "192.168.1.150/23")
	mgr := seedGatewayLease(t, "vpc-1", "192.168.1.150")

	reconcileGatewayLeases(context.Background(), mgr, newHookIGWManager(t, m))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.150/23"}, lrp.Networks)
}

// An EIP lease has no gateway port, so the reconcile must not touch one.
func TestReconcileGatewayLeasesIgnoresOtherPurposes(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store, err := dhcp.NewStore(t.Context(), js, "az1")
	require.NoError(t, err)
	require.NoError(t, store.Put(t.Context(), dhcp.Entry{
		Purpose: dhcp.PurposeEIP,
		Lease:   &dhcp.Lease{ClientID: "eipalloc-1", IP: net.ParseIP("192.168.1.5")},
	}))
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{Client: dhcp.NewFake(), Store: store})
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	m := gatewayRouterWithPort(t, "vpc-1", "192.168.1.240/23")
	reconcileGatewayLeases(context.Background(), mgr, newHookIGWManager(t, m))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.240/23"}, lrp.Networks)
}

// A missing port errors inside RebindGatewayIP; the sweep must carry on to the
// remaining leases rather than abort on the first failure.
func TestReconcileGatewayLeasesContinuesPastFailure(t *testing.T) {
	m := gatewayRouterWithPort(t, "vpc-2", "192.168.1.240/23")
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store, err := dhcp.NewStore(t.Context(), js, "az1")
	require.NoError(t, err)
	for _, vpcID := range []string{"vpc-missing", "vpc-2"} {
		require.NoError(t, store.Put(t.Context(), dhcp.Entry{
			Purpose: dhcp.PurposeGatewayLRP,
			VPCID:   vpcID,
			Lease: &dhcp.Lease{
				ClientID:   dhcp.GatewayLRPClientID(vpcID),
				IP:         net.ParseIP("192.168.1.150"),
				SubnetMask: net.CIDRMask(23, 32),
			},
		}))
	}
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{Client: dhcp.NewFake(), Store: store})
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	reconcileGatewayLeases(context.Background(), mgr, newHookIGWManager(t, m))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-2"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.150/23"}, lrp.Networks)
}

func TestReconcileGatewayLeasesWithoutManagerIsNoOp(t *testing.T) {
	m := gatewayRouterWithPort(t, "vpc-1", "192.168.1.240/23")
	reconcileGatewayLeases(context.Background(), nil, newHookIGWManager(t, m))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.240/23"}, lrp.Networks)
}
