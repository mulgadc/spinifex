package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// newTestDHCPManager spins up an embedded JetStream, wires a Fake client,
// and returns the Manager along with the NATS connection. Cleanup of NATS
// is handled by testutil; the Manager is closed via t.Cleanup.
func newTestDHCPManager(t *testing.T, opts ...DHCPManagerOption) (*DHCPManager, *nats.Conn, *dhcp.Fake) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	fake := dhcp.NewFake()
	m, err := NewDHCPManager(nc, js, fake, opts...)
	require.NoError(t, err)
	t.Cleanup(m.Close)
	subs, err := m.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	return m, nc, fake
}

func requestAcquire(t *testing.T, nc *nats.Conn, req dhcp.AcquireRequestMsg) dhcp.AcquireReplyMsg {
	t.Helper()
	data, err := json.Marshal(req)
	require.NoError(t, err)
	msg, err := nc.Request(dhcp.TopicAcquire, data, 5*time.Second)
	require.NoError(t, err)
	var reply dhcp.AcquireReplyMsg
	require.NoError(t, json.Unmarshal(msg.Data, &reply))
	return reply
}

func requestRelease(t *testing.T, nc *nats.Conn, clientID string) dhcp.ReleaseReplyMsg {
	t.Helper()
	data, err := json.Marshal(dhcp.ReleaseRequestMsg{ClientID: clientID})
	require.NoError(t, err)
	msg, err := nc.Request(dhcp.TopicRelease, data, 5*time.Second)
	require.NoError(t, err)
	var reply dhcp.ReleaseReplyMsg
	require.NoError(t, json.Unmarshal(msg.Data, &reply))
	return reply
}

func TestDHCPManager_AcquireReturnsLease(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{
		Bridge: "br-wan", ClientID: "eni-1", Hostname: "eni-1", VendorClass: "i-abc",
	})
	require.Empty(t, reply.Error, "unexpected error: %s", reply.Error)
	require.Equal(t, "192.0.2.100", reply.IP)
	require.NotEmpty(t, reply.ServerID)
	require.NotZero(t, reply.ExpiresUnix)
	require.NotEmpty(t, reply.HWAddr)
	require.Equal(t, 1, fake.AcquireCount())
}

func TestDHCPManager_AcquireIdempotentWhileLeaseActive(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	req := dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-idem"}

	first := requestAcquire(t, nc, req)
	require.Empty(t, first.Error)
	second := requestAcquire(t, nc, req)
	require.Empty(t, second.Error)

	require.Equal(t, first.IP, second.IP, "idempotent acquire should return the same IP")
	require.Equal(t, 1, fake.AcquireCount(), "second request must not trigger a fresh DORA")
}

func TestDHCPManager_AcquireFailureSurfacesError(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	fake.AcquireHook = func(dhcp.AcquireRequest) (*dhcp.Lease, error) {
		return nil, errors.New("server unreachable")
	}
	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-err"})
	require.Contains(t, reply.Error, "server unreachable")
	require.Empty(t, reply.IP)
}

func TestDHCPManager_ReleaseRemovesLease(t *testing.T) {
	m, nc, fake := newTestDHCPManager(t)
	req := dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-release"}
	require.Empty(t, requestAcquire(t, nc, req).Error)

	reply := requestRelease(t, nc, req.ClientID)
	require.Empty(t, reply.Error)
	require.Equal(t, 1, fake.ReleaseCount())

	_, ok := m.get("eni-release")
	require.False(t, ok, "lease should be forgotten after Release")
}

func TestDHCPManager_ReleaseUnknownIsNoop(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	reply := requestRelease(t, nc, "nonexistent")
	require.Empty(t, reply.Error)
	require.Equal(t, 0, fake.ReleaseCount())
}

func TestDHCPManager_AcquireRejectsMissingFields(t *testing.T) {
	_, nc, _ := newTestDHCPManager(t)
	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "", ClientID: "eni-x"})
	require.Contains(t, reply.Error, "bridge and client_id are required")
}

func TestDHCPManager_HWAddrDerivedFromClientID(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-mac"})
	require.Empty(t, reply.Error)
	lease, ok := fake.HeldLease("eni-mac")
	require.True(t, ok)
	// generateMAC uses the 02:00:00 locally-administered unicast prefix.
	require.Equal(t, "02:00:00", lease.HWAddr.String()[:8])
}

func TestDHCPManager_HWAddrExplicitlySuppliedIsUsed(t *testing.T) {
	_, nc, fake := newTestDHCPManager(t)
	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{
		Bridge: "br-wan", ClientID: "eni-mac-explicit", HWAddr: "02:aa:bb:cc:dd:ee",
	})
	require.Empty(t, reply.Error)
	lease, _ := fake.HeldLease("eni-mac-explicit")
	require.Equal(t, "02:aa:bb:cc:dd:ee", lease.HWAddr.String())
}

func TestDHCPManager_RenewalFiresAndUpdatesLease(t *testing.T) {
	tmpl := dhcp.DefaultLeaseTemplate()
	tmpl.LeaseDuration = 40 * time.Millisecond
	fake := dhcp.NewFake()
	fake.SetDefaultLease(tmpl)

	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewDHCPManager(nc, js, fake, WithDHCPJitterFraction(0))
	require.NoError(t, err)
	t.Cleanup(m.Close)
	subs, err := m.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-renew"})
	require.Empty(t, reply.Error)

	// T1 = lease/2 = 20 ms. Wait a few multiples to catch at least one renew.
	require.Eventually(t, func() bool { return fake.RenewCount() >= 1 }, 2*time.Second, 10*time.Millisecond)
}

func TestDHCPManager_RenewFailureEmitsLeaseExpired(t *testing.T) {
	tmpl := dhcp.DefaultLeaseTemplate()
	tmpl.LeaseDuration = 60 * time.Millisecond
	fake := dhcp.NewFake()
	fake.SetDefaultLease(tmpl)
	fake.RenewHook = func(*dhcp.Lease) (*dhcp.Lease, error) {
		return nil, errors.New("server NAK")
	}

	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewDHCPManager(nc, js, fake, WithDHCPJitterFraction(0))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	var events atomic.Int32
	expiredMsg := make(chan dhcp.LeaseExpiredEvent, 1)
	sub, err := nc.Subscribe(dhcp.TopicLeaseExpired, func(msg *nats.Msg) {
		events.Add(1)
		var evt dhcp.LeaseExpiredEvent
		if err := json.Unmarshal(msg.Data, &evt); err == nil {
			select {
			case expiredMsg <- evt:
			default:
			}
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	subs, err := m.Subscribe(nc)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})

	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-expire"})
	require.Empty(t, reply.Error)

	// T1=30ms → try, fail. T2=~52ms → try, fail. Expires at 60ms.
	select {
	case evt := <-expiredMsg:
		require.Equal(t, "eni-expire", evt.ClientID)
		require.Equal(t, "nak", evt.Reason)
	case <-time.After(2 * time.Second):
		t.Fatalf("expected lease-expired event (events=%d)", events.Load())
	}
}

func TestDHCPManager_BootstrapRehydratesLease(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	fake := dhcp.NewFake()

	m1, err := NewDHCPManager(nc, js, fake)
	require.NoError(t, err)
	subs, err := m1.Subscribe(nc)
	require.NoError(t, err)

	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-boot"})
	require.Empty(t, reply.Error)

	// Close first manager; KV entry should survive.
	for _, s := range subs {
		_ = s.Unsubscribe()
	}
	m1.Close()

	// Second manager picks up the lease on Bootstrap.
	fake2 := dhcp.NewFake()
	m2, err := NewDHCPManager(nc, js, fake2)
	require.NoError(t, err)
	t.Cleanup(m2.Close)
	require.NoError(t, m2.Bootstrap(context.Background()))

	lease, ok := m2.get("eni-boot")
	require.True(t, ok, "manager should rehydrate persisted lease")
	require.Equal(t, "192.0.2.100", lease.IP.String())
	require.Equal(t, 0, fake2.AcquireCount(), "no fresh DORA on bootstrap")
}

func TestDHCPManager_BootstrapDropsExpiredLeases(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	fake := dhcp.NewFake()

	m1, err := NewDHCPManager(nc, js, fake)
	require.NoError(t, err)
	subs, err := m1.Subscribe(nc)
	require.NoError(t, err)

	reply := requestAcquire(t, nc, dhcp.AcquireRequestMsg{Bridge: "br-wan", ClientID: "eni-stale"})
	require.Empty(t, reply.Error)

	for _, s := range subs {
		_ = s.Unsubscribe()
	}
	m1.Close()

	// Rewrite the persisted record so AcquiredAt + LeaseDuration is in the past.
	kv, err := js.KeyValue(dhcp.KVBucketDHCPLeases)
	require.NoError(t, err)
	entry, err := kv.Get("eni-stale")
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	rec["acquired_unix"] = time.Now().Add(-2 * time.Hour).Unix()
	rec["lease_seconds"] = int64(60)
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Put("eni-stale", raw)
	require.NoError(t, err)

	fake2 := dhcp.NewFake()
	m2, err := NewDHCPManager(nc, js, fake2)
	require.NoError(t, err)
	t.Cleanup(m2.Close)

	var expired sync.WaitGroup
	expired.Add(1)
	sub, err := nc.Subscribe(dhcp.TopicLeaseExpired, func(msg *nats.Msg) {
		var evt dhcp.LeaseExpiredEvent
		if err := json.Unmarshal(msg.Data, &evt); err == nil && evt.ClientID == "eni-stale" {
			expired.Done()
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, m2.Bootstrap(context.Background()))

	done := make(chan struct{})
	go func() { expired.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected lease-expired event for stale lease")
	}
	_, err = kv.Get("eni-stale")
	require.ErrorIs(t, err, nats.ErrKeyNotFound)
}

func TestDHCPManager_JitterWithinBounds(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewDHCPManager(nc, js, dhcp.NewFake(), WithDHCPJitterFraction(0.1))
	require.NoError(t, err)
	t.Cleanup(m.Close)

	base := 100 * time.Millisecond
	minOK := time.Duration(float64(base) * 0.9)
	maxOK := time.Duration(float64(base) * 1.1)
	for i := range 100 {
		got := m.jitter(base)
		if got < minOK || got > maxOK {
			t.Fatalf("iteration %d: jitter produced %v, outside [%v,%v]", i, got, minOK, maxOK)
		}
	}
}

func TestNewDHCPManager_RejectsNilDeps(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	_, err := NewDHCPManager(nil, js, dhcp.NewFake())
	require.ErrorContains(t, err, "nats.Conn is required")

	_, err = NewDHCPManager(nc, nil, dhcp.NewFake())
	require.ErrorContains(t, err, "JetStreamContext is required")

	_, err = NewDHCPManager(nc, js, nil)
	require.ErrorContains(t, err, "dhcp.Client is required")
}

func TestDHCPManager_OptionsApply(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	customMAC := func(string) net.HardwareAddr {
		mac, _ := net.ParseMAC("02:de:ad:be:ef:01")
		return mac
	}
	m, err := NewDHCPManager(nc, js, dhcp.NewFake(),
		WithDHCPMACFunc(customMAC),
		WithDHCPAcquireTimeout(1*time.Second),
		WithDHCPJitterFraction(0.05),
	)
	require.NoError(t, err)
	t.Cleanup(m.Close)
	require.Equal(t, time.Second, m.acquireTimeout)
	require.InDelta(t, 0.05, m.jitterFraction, 0.0001)
	require.Equal(t, "02:de:ad:be:ef:01", m.macForClientID("anything").String())
}

func TestDHCPManager_BootstrapEmptyBucket(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewDHCPManager(nc, js, dhcp.NewFake())
	require.NoError(t, err)
	t.Cleanup(m.Close)

	// No leases in KV → Bootstrap is a clean no-op.
	require.NoError(t, m.Bootstrap(context.Background()))
}

func TestDHCPManager_JitterWithZeroFractionReturnsInput(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewDHCPManager(nc, js, dhcp.NewFake(), WithDHCPJitterFraction(0))
	require.NoError(t, err)
	t.Cleanup(m.Close)
	require.Equal(t, 100*time.Millisecond, m.jitter(100*time.Millisecond))
	require.Equal(t, time.Duration(0), m.jitter(0))
}

func TestDHCPManager_ClassifyRenewErr(t *testing.T) {
	require.Equal(t, "unknown", classifyRenewErr(nil))
	require.Equal(t, "nak", classifyRenewErr(errors.New("got NAK from server")))
	require.Equal(t, "server_unreachable", classifyRenewErr(errors.New("i/o timeout")))
	require.Equal(t, "renew_failed", classifyRenewErr(errors.New("something else")))
}

func TestDHCPManager_EncodeDecodeLeaseRoundTrip(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:de:ad:be")
	in := &dhcp.Lease{
		Bridge: "br-wan", ClientID: "eni-rt", Hostname: "eni-rt", VendorClass: "i-xyz",
		HWAddr:        mac,
		IP:            net.ParseIP("192.0.2.50"),
		SubnetMask:    net.CIDRMask(24, 32),
		Routers:       []net.IP{net.ParseIP("192.0.2.1")},
		DNS:           []net.IP{net.ParseIP("1.1.1.1")},
		ServerID:      net.ParseIP("192.0.2.1"),
		AcquiredAt:    time.Unix(1_700_000_000, 0).UTC(),
		LeaseDuration: time.Hour,
		T1:            30 * time.Minute,
		T2:            52 * time.Minute,
		RawOffer:      []byte{0x01, 0x02},
		RawACK:        []byte{0x03, 0x04},
	}
	data, err := encodeLease(in)
	require.NoError(t, err)
	out, err := decodeLease(data)
	require.NoError(t, err)

	require.Equal(t, in.ClientID, out.ClientID)
	require.Equal(t, in.IP.String(), out.IP.String())
	require.Equal(t, in.LeaseDuration, out.LeaseDuration)
	require.Equal(t, in.HWAddr.String(), out.HWAddr.String())
	require.Equal(t, in.RawACK, out.RawACK)
	require.Equal(t, in.RawOffer, out.RawOffer)
}
