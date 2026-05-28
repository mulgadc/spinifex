package dhcp_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestManager(t *testing.T, az string, fake *dhcp.Fake, now func() time.Time) (*dhcp.Manager, *dhcp.Store, *nats.Conn) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	store, err := dhcp.NewStore(js, az)
	require.NoError(t, err)
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{Client: fake, Store: store, Now: now})
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)
	return mgr, store, nc
}

func TestManagerNewRequiresClientAndStore(t *testing.T) {
	_, err := dhcp.NewManager(dhcp.ManagerConfig{})
	assert.Error(t, err)
	_, err = dhcp.NewManager(dhcp.ManagerConfig{Client: dhcp.NewFake()})
	assert.Error(t, err)
}

func TestManagerStartScansAndReaffirms(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "az1", fake, time.Now)

	// Pre-populate KV with two BOUND leases. Manager.Start should adopt
	// both and call Renew on each (RFC 2131 §4.3.2 INIT-REBOOT
	// equivalent — fake.Renew counts every reaffirm).
	for _, id := range []string{"eipalloc-a", "eipalloc-b"} {
		hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
		_, err := fake.Acquire(context.Background(), dhcp.AcquireRequest{
			Bridge: "br-wan", ClientID: id, HWAddr: hw,
		})
		require.NoError(t, err)
		held, _ := fake.HeldLease(id)
		require.NoError(t, store.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: held}))
	}
	require.Equal(t, 0, fake.RenewCount())

	require.NoError(t, mgr.Start(context.Background()))
	require.Eventually(t, func() bool { return fake.RenewCount() == 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, 2, mgr.LoopCount())
}

func TestManagerStartDropsExpiredLeases(t *testing.T) {
	fake := dhcp.NewFake()
	fixed := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	mgr, store, _ := newTestManager(t, "az1", fake, func() time.Time { return fixed })

	hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	expired := &dhcp.Lease{
		Bridge:        "br-wan",
		ClientID:      "eipalloc-old",
		HWAddr:        hw,
		IP:            net.IPv4(192, 0, 2, 50),
		AcquiredAt:    fixed.Add(-2 * time.Hour),
		LeaseDuration: time.Hour,
	}
	require.NoError(t, store.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: expired}))

	require.NoError(t, mgr.Start(context.Background()))
	_, err := store.Get("eipalloc-old")
	assert.ErrorIs(t, err, nats.ErrKeyNotFound)
	assert.Equal(t, 0, fake.RenewCount())
	assert.Equal(t, 0, mgr.LoopCount())
}

func TestManagerStartTwiceErrors(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, _, _ := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	err := mgr.Start(context.Background())
	assert.Error(t, err)
}

func TestNATSClientAcquireReleaseRoundtrip(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	lease, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
		Bridge: "br-wan", ClientID: "eipalloc-A", HWAddr: hw,
		Purpose: "eip", PoolName: "default",
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "eipalloc-A", lease.ClientID)
	assert.NotNil(t, lease.IP)

	got, err := store.Get("eipalloc-A")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "eip", got.Purpose)
	assert.Equal(t, "default", got.PoolName)

	require.NoError(t, client.RequestRelease(context.Background(), "eipalloc-A"))
	_, err = store.Get("eipalloc-A")
	assert.ErrorIs(t, err, nats.ErrKeyNotFound)
	assert.Equal(t, 1, fake.ReleaseCount())
}

func TestNATSClientAcquireIdempotent(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, _, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	client := dhcp.NewNATSClient(nc, 2*time.Second)
	hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	params := dhcp.AcquireParams{
		Bridge: "br-wan", ClientID: "eipalloc-idem", HWAddr: hw,
		Purpose: "eip", PoolName: "default",
	}
	first, err := client.RequestAcquire(context.Background(), params)
	require.NoError(t, err)
	second, err := client.RequestAcquire(context.Background(), params)
	require.NoError(t, err)
	assert.True(t, first.IP.Equal(second.IP), "second acquire returned a different IP: %v vs %v", first.IP, second.IP)
	assert.Equal(t, 1, fake.AcquireCount(), "idempotency should yield exactly one wire DORA")
}

func TestNATSClientAcquireConcurrent(t *testing.T) {
	fake := dhcp.NewFake()
	// Block the first Acquire until both callers have raced past the
	// KV-existence check; the second caller must coalesce through
	// singleflight rather than launching its own DORA.
	gate := make(chan struct{})
	var firstHit atomic.Int32
	fake.AcquireHook = func(req dhcp.AcquireRequest) (*dhcp.Lease, error) {
		if firstHit.Add(1) == 1 {
			<-gate
		}
		mac, _ := net.ParseMAC("02:00:00:aa:bb:cc")
		return &dhcp.Lease{
			Bridge:        req.Bridge,
			ClientID:      req.ClientID,
			HWAddr:        mac,
			IP:            net.IPv4(192, 0, 2, 100),
			SubnetMask:    net.CIDRMask(24, 32),
			ServerID:      net.IPv4(192, 0, 2, 1),
			AcquiredAt:    time.Now(),
			LeaseDuration: time.Hour,
			T1:            30 * time.Minute,
			T2:            52*time.Minute + 30*time.Second,
			RawOffer:      []byte("fake-offer"),
			RawACK:        []byte("fake-ack"),
		}, nil
	}

	mgr, _, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	client := dhcp.NewNATSClient(nc, 3*time.Second)
	hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	params := dhcp.AcquireParams{Bridge: "br-wan", ClientID: "eipalloc-race", HWAddr: hw, Purpose: "eip"}

	var wg sync.WaitGroup
	results := make([]*dhcp.Lease, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = client.RequestAcquire(context.Background(), params)
		}(i)
	}
	// Let the first DORA "start"; gate is closed below so the in-flight
	// Acquire completes. Singleflight collapses the second caller into
	// the first.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.True(t, results[0].IP.Equal(results[1].IP))
	assert.Equal(t, int32(1), firstHit.Load(), "exactly one wire DORA expected")
}

func TestManagerHandleAcquireRequiresClientID(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, _, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	client := dhcp.NewNATSClient(nc, time.Second)
	_, err = client.RequestAcquire(context.Background(), dhcp.AcquireParams{Bridge: "br-wan"})
	assert.Error(t, err)
}

func TestManagerReleaseUnknownClientIsNoop(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, _, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	client := dhcp.NewNATSClient(nc, time.Second)
	assert.NoError(t, client.RequestRelease(context.Background(), "never-existed"))
	assert.Equal(t, 0, fake.ReleaseCount())
}

func TestManagerStopCancelsLoops(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, _, nc := newTestManager(t, "az1", fake, time.Now)
	require.NoError(t, mgr.Start(context.Background()))
	subs, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	client := dhcp.NewNATSClient(nc, time.Second)
	hw, _ := net.ParseMAC("02:00:00:aa:bb:cc")
	for _, id := range []string{"a", "b", "c"} {
		_, err := client.RequestAcquire(context.Background(), dhcp.AcquireParams{
			Bridge: "br-wan", ClientID: id, HWAddr: hw, Purpose: "eip",
		})
		require.NoError(t, err)
	}
	assert.Equal(t, 3, mgr.LoopCount())

	mgr.Stop()
	assert.Equal(t, 0, mgr.LoopCount())
}
