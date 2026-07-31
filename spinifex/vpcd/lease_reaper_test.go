package vpcd

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// respondOwnerCheck stands in for the daemon, returning a fixed verdict and
// recording what it was asked.
func serveOwnerCheck(t *testing.T, nc *nats.Conn, reply dhcp.OwnerCheckReply) *dhcp.OwnerCheckRequest {
	t.Helper()
	seen := &dhcp.OwnerCheckRequest{}
	sub, err := nc.Subscribe(dhcp.TopicOwnerCheck, func(msg *nats.Msg) {
		_ = json.Unmarshal(msg.Data, seen)
		data, _ := json.Marshal(reply)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return seen
}

func eipLeaseEntry(clientID, ip string) dhcp.Entry {
	return dhcp.Entry{
		Purpose: dhcp.PurposeEIP,
		Lease:   &dhcp.Lease{ClientID: clientID, IP: net.ParseIP(ip)},
	}
}

func TestLeaseOwnerResolverReportsGone(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seen := serveOwnerCheck(t, nc, dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusGone})

	resolver := &leaseOwnerResolver{nc: nc}
	status, err := resolver.Status(context.Background(), eipLeaseEntry("eipalloc-1", "192.168.1.5"))
	require.NoError(t, err)
	assert.Equal(t, dhcp.OwnerGone, status)
	assert.Equal(t, "eipalloc-1", seen.ClientID)
	assert.Equal(t, dhcp.PurposeEIP, seen.Purpose)
}

func TestLeaseOwnerResolverReportsAlive(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveOwnerCheck(t, nc, dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusAlive})

	resolver := &leaseOwnerResolver{nc: nc}
	status, err := resolver.Status(context.Background(), eipLeaseEntry("eipalloc-1", "192.168.1.5"))
	require.NoError(t, err)
	assert.Equal(t, dhcp.OwnerAlive, status)
}

// No responder must read as unknown, never as gone: an unreachable daemon would
// otherwise release every address the cluster holds.
func TestLeaseOwnerResolverUnansweredIsUnknown(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	resolver := &leaseOwnerResolver{nc: nc}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	status, err := resolver.Status(ctx, eipLeaseEntry("eipalloc-1", "192.168.1.5"))
	require.Error(t, err)
	assert.Equal(t, dhcp.OwnerUnknown, status)
}

// A responder that reports a lookup failure alongside a status must not be read
// as a verdict.
func TestLeaseOwnerResolverSurfacesResponderError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveOwnerCheck(t, nc, dhcp.OwnerCheckReply{Status: dhcp.OwnerStatusUnknown, Error: "KV timeout"})

	resolver := &leaseOwnerResolver{nc: nc}
	status, err := resolver.Status(context.Background(), eipLeaseEntry("eipalloc-1", "192.168.1.5"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KV timeout")
	assert.Equal(t, dhcp.OwnerUnknown, status)
}

func TestLeaseOwnerResolverRejectsNilLease(t *testing.T) {
	resolver := &leaseOwnerResolver{}
	status, err := resolver.Status(context.Background(), dhcp.Entry{Purpose: dhcp.PurposeEIP})
	require.Error(t, err)
	assert.Equal(t, dhcp.OwnerUnknown, status)
}

// A gateway lease configured a router port, so the port goes before the address
// does — otherwise OVN keeps answering ARP for an address back in the pool.
func TestLeaseOwnerResolverDiscardDetachesGateway(t *testing.T) {
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(context.Background(), &nbdb.LogicalRouter{
		Name:        topology.VPCRouter("vpc-1"),
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1", "spinifex:cidr": "172.31.0.0/16"},
	}))
	require.NoError(t, m.CreateLogicalRouterPort(context.Background(), topology.VPCRouter("vpc-1"), &nbdb.LogicalRouterPort{
		Name:     topology.GatewayRouterPort("vpc-1"),
		Networks: []string{"192.168.1.150/23"},
	}))

	resolver := &leaseOwnerResolver{igwMgr: newHookIGWManager(t, m)}
	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "vpc-1", "dhcp-gw-lrp-vpc-1", "192.168.1.150")
	require.NoError(t, resolver.Discard(context.Background(), entry))

	_, err := m.GetLogicalRouterPort(context.Background(), topology.GatewayRouterPort("vpc-1"))
	require.Error(t, err, "the orphaned gateway port must be gone before the address is released")
	assert.Contains(t, err.Error(), "not found")
}

// An EIP or ENI-public address has no datapath of its own left to remove: it
// went with the record that named it.
func TestLeaseOwnerResolverDiscardIsNoOpForNonGateway(t *testing.T) {
	resolver := &leaseOwnerResolver{}
	require.NoError(t, resolver.Discard(context.Background(), eipLeaseEntry("eipalloc-1", "192.168.1.5")))
}

func TestLeaseOwnerResolverDiscardRejectsGatewayWithoutVPC(t *testing.T) {
	resolver := &leaseOwnerResolver{igwMgr: newHookIGWManager(t, mock.New())}
	entry := gwLeaseEntry(dhcp.PurposeGatewayLRP, "", "dhcp-gw-lrp-vpc-1", "192.168.1.150")
	require.Error(t, resolver.Discard(context.Background(), entry))
}

func TestRunLeaseReaperStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLeaseReaper(ctx, nil, time.Millisecond, time.Millisecond)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not stop when its context ended")
	}
}
