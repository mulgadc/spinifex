package dhcp_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newIfaceMACManager(t *testing.T, fake *dhcp.Fake, ifaceIPs func(string) ([]net.IP, error)) (*dhcp.Manager, *dhcp.Store, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store, err := dhcp.NewStore(t.Context(), js, "az1")
	require.NoError(t, err)
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{Client: fake, Store: store, IfaceIPs: ifaceIPs})
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	return mgr, store, nc
}

func noIfaceIPs(string) ([]net.IP, error) { return nil, nil }

// UseIfaceMAC must survive the NATS wire, reach the Client request, and
// persist through the KV entry so re-acquire after restart repeats it.
func TestAcquireUseIfaceMACThreadedAndPersisted(t *testing.T) {
	fake := dhcp.NewFake()
	var gotReq dhcp.AcquireRequest
	fake.AcquireHook = func(req dhcp.AcquireRequest) (*dhcp.Lease, error) {
		gotReq = req
		return &dhcp.Lease{
			Bridge: req.Bridge, ClientID: req.ClientID, UseIfaceMAC: req.UseIfaceMAC,
			IP: net.IPv4(192, 0, 2, 100), AcquiredAt: time.Now(), LeaseDuration: time.Hour,
		}, nil
	}
	_, store, nc := newIfaceMACManager(t, fake, noIfaceIPs)

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	lease, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "wlan0", ClientID: "eipalloc-A", UseIfaceMAC: true, PoolName: "wan",
	})
	require.NoError(t, err)
	assert.True(t, gotReq.UseIfaceMAC, "flag must reach the DHCP client")
	assert.True(t, lease.UseIfaceMAC, "flag must reflect back on the lease")

	got, err := store.Get(t.Context(), "eipalloc-A")
	require.NoError(t, err)
	assert.True(t, got.Lease.UseIfaceMAC, "flag must round-trip through KV")
}

// Derived-MAC acquires (the default) must not set the flag anywhere.
func TestAcquireDerivedMACDefault(t *testing.T) {
	fake := dhcp.NewFake()
	_, store, nc := newIfaceMACManager(t, fake, noIfaceIPs)

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	lease, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "br-wan", ClientID: "eipalloc-B", PoolName: "wan",
	})
	require.NoError(t, err)
	assert.False(t, lease.UseIfaceMAC)
	got, err := store.Get(t.Context(), "eipalloc-B")
	require.NoError(t, err)
	assert.False(t, got.Lease.UseIfaceMAC)
}

// A MAC-keyed upstream router ACKs the interface's own IP back to an
// interface-MAC client. That lease is unusable — hard error, lease released.
func TestAcquireIfaceMACCollisionWithOwnIP(t *testing.T) {
	fake := dhcp.NewFake()
	tmpl := dhcp.DefaultLeaseTemplate() // leases 192.0.2.100
	fake.SetDefaultLease(tmpl)
	ownIPs := func(iface string) ([]net.IP, error) {
		return []net.IP{net.IPv4(192, 0, 2, 100)}, nil
	}
	_, store, nc := newIfaceMACManager(t, fake, ownIPs)

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	_, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "wlan0", ClientID: "eipalloc-C", UseIfaceMAC: true, PoolName: "wan",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `use source="static"`)
	assert.Equal(t, 1, fake.ReleaseCount(), "colliding lease must be released")
	_, err = store.Get(t.Context(), "eipalloc-C")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "colliding lease must not persist")
}

// Two client-ids ACKing the same IP means the router ignores option 61.
func TestAcquireIfaceMACCollisionWithExistingLease(t *testing.T) {
	fake := dhcp.NewFake() // template always leases 192.0.2.100
	_, _, nc := newIfaceMACManager(t, fake, noIfaceIPs)

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	_, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "wlan0", ClientID: "eipalloc-D", UseIfaceMAC: true, PoolName: "wan",
	})
	require.NoError(t, err, "first lease is fine")

	_, err = client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "wlan0", ClientID: "eipalloc-E", UseIfaceMAC: true, PoolName: "wan",
	})
	require.Error(t, err, "second client-id got the same IP — MAC-keyed router")
	assert.Contains(t, err.Error(), `use source="static"`)
	assert.Contains(t, err.Error(), "eipalloc-D")
}

// A dhcp_mac="interface" pool must set UseIfaceMAC on every acquire.
func TestDHCPPoolAllocatorInterfaceMACPool(t *testing.T) {
	fake := dhcp.NewFake()
	var gotReq dhcp.AcquireRequest
	fake.AcquireHook = func(req dhcp.AcquireRequest) (*dhcp.Lease, error) {
		gotReq = req
		return &dhcp.Lease{
			Bridge: req.Bridge, ClientID: req.ClientID, UseIfaceMAC: req.UseIfaceMAC,
			IP: net.IPv4(192, 0, 2, 100), AcquiredAt: time.Now(), LeaseDuration: time.Hour,
		}, nil
	}
	pool := external.ExternalPoolConfig{
		Name: "wan", Source: external.SourceDHCP, BindBridge: "wlan0",
		DHCPMAC: external.DHCPMACInterface,
	}
	allocator, _ := newPoolAllocator(t, fake, pool)

	_, err := allocator.Allocate(context.Background(), external.AllocateRequest{
		PoolName: "wan", AllocationID: "eipalloc-H",
	})
	require.NoError(t, err)
	assert.True(t, gotReq.UseIfaceMAC)
}

// Distinct IPs per client-id (a sane server) must not trip the detector.
func TestAcquireIfaceMACDistinctIPsOK(t *testing.T) {
	fake := dhcp.NewFake()
	next := byte(10)
	fake.AcquireHook = func(req dhcp.AcquireRequest) (*dhcp.Lease, error) {
		ip := net.IPv4(192, 0, 2, next)
		next++
		return &dhcp.Lease{
			Bridge: req.Bridge, ClientID: req.ClientID, UseIfaceMAC: req.UseIfaceMAC,
			IP: ip, AcquiredAt: time.Now(), LeaseDuration: time.Hour,
		}, nil
	}
	_, _, nc := newIfaceMACManager(t, fake, noIfaceIPs)

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	for _, id := range []string{"eipalloc-F", "eipalloc-G"} {
		_, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
			Bridge: "wlan0", ClientID: id, UseIfaceMAC: true, PoolName: "wan",
		})
		require.NoError(t, err, id)
	}
}
