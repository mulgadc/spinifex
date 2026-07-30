package vpcd

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
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

	// nil conn: a gateway lease is rebound in-process, never over NATS.
	hook := leaseIPChangeHook(newHookIGWManager(t, m), nil)
	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "vpc-1", "dhcp-gw-lrp-vpc-1", "192.168.1.146")
	require.NoError(t, hook(context.Background(), entry, net.ParseIP("192.168.1.115")))

	lrp, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.146/23"}, lrp.Networks)
}

// EIP and ENI-public addresses are API-visible records owned daemon-side, so
// the hook must hand the move over rather than repoint a datapath while
// DescribeAddresses still returns the old IP.
func TestLeaseIPChangeHookRequestsRecordRebind(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	got := make(chan dhcp.LeaseChangedRequest, 1)
	sub, err := nc.Subscribe(dhcp.TopicLeaseChanged, func(msg *nats.Msg) {
		var req dhcp.LeaseChangedRequest
		require.NoError(t, json.Unmarshal(msg.Data, &req))
		got <- req
		reply, _ := json.Marshal(dhcp.LeaseChangedReply{})
		require.NoError(t, msg.Respond(reply))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()), nc)
	entry := gwLeaseEntry(dhcp.PurposeEIP, "vpc-1", "eipalloc-1", "192.168.1.146")
	entry.PoolName = "wan"
	require.NoError(t, hook(context.Background(), entry, net.ParseIP("192.168.1.115")))

	select {
	case req := <-got:
		assert.Equal(t, "eipalloc-1", req.ClientID)
		assert.Equal(t, dhcp.PurposeEIP, req.Purpose)
		assert.Equal(t, "wan", req.PoolName)
		assert.Equal(t, "192.168.1.115", req.OldIP)
		assert.Equal(t, "192.168.1.146", req.NewIP)
	case <-time.After(5 * time.Second):
		t.Fatal("no lease-changed request published")
	}
}

// A rejected rebind means the record still names a released address, so the
// hook must not report success.
func TestLeaseIPChangeHookSurfacesRebindRejection(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	sub, err := nc.Subscribe(dhcp.TopicLeaseChanged, func(msg *nats.Msg) {
		reply, _ := json.Marshal(dhcp.LeaseChangedReply{Error: "no EIP record for allocation eipalloc-1"})
		require.NoError(t, msg.Respond(reply))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()), nc)
	entry := gwLeaseEntry(dhcp.PurposeEIP, "", "eipalloc-1", "192.168.1.146")
	err = hook(context.Background(), entry, net.ParseIP("192.168.1.115"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no EIP record for allocation eipalloc-1")
}

// No responder must not read as success either — nothing moved the record.
func TestLeaseIPChangeHookErrorsWithoutResponder(t *testing.T) {
	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()), nil)

	entry := gwLeaseEntry(dhcp.PurposeENIPublic, "", "eni-1", "192.168.1.146")
	err := hook(context.Background(), entry, net.ParseIP("192.168.1.115"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eni-1")
}

func TestLeaseIPChangeHookRejectsGatewayLeaseWithoutVPC(t *testing.T) {
	hook := leaseIPChangeHook(newHookIGWManager(t, mock.New()), nil)

	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "", "dhcp-gw-lrp-vpc-1", "192.168.1.146")
	err := hook(context.Background(), entry, net.ParseIP("192.168.1.115"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no vpc_id")
}
