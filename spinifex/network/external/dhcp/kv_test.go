package dhcp_test

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T, az string) *dhcp.Store {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	s, err := dhcp.NewStore(js, az)
	require.NoError(t, err)
	return s
}

func sampleLease(clientID string) *dhcp.Lease {
	mac, _ := net.ParseMAC("52:54:00:aa:bb:cc")
	acq := time.Date(2026, 5, 28, 8, 30, 0, 0, time.UTC)
	return &dhcp.Lease{
		Bridge:        "br-wan",
		ClientID:      clientID,
		Hostname:      "node-a",
		VendorClass:   "mulga-spinifex-gw",
		HWAddr:        mac,
		IP:            net.IPv4(192, 168, 0, 123),
		SubnetMask:    net.CIDRMask(24, 32),
		Routers:       []net.IP{net.IPv4(192, 168, 0, 1)},
		DNS:           []net.IP{net.IPv4(192, 168, 0, 1)},
		ServerID:      net.IPv4(192, 168, 0, 1),
		AcquiredAt:    acq,
		LeaseDuration: time.Hour,
		T1:            30 * time.Minute,
		T2:            52*time.Minute + 30*time.Second,
		RawOffer:      []byte("offer-bytes"),
		RawACK:        []byte("ack-bytes"),
	}
}

func TestBucketNamePerAZ(t *testing.T) {
	assert.Equal(t, "spinifex-dhcp-leases-ap-southeast-2a", dhcp.BucketName("ap-southeast-2a"))
	assert.Equal(t, "spinifex-dhcp-leases", dhcp.BucketName(""))
}

func TestStorePutGetRoundtrip(t *testing.T) {
	s := newTestStore(t, "az1")
	e := dhcp.Entry{
		Purpose:  "eip",
		PoolName: "default",
		Lease:    sampleLease("eipalloc-0a1b2c3d"),
	}
	require.NoError(t, s.Put(e))

	got, err := s.Get("eipalloc-0a1b2c3d")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, e.Purpose, got.Purpose)
	assert.Equal(t, e.PoolName, got.PoolName)
	assert.Equal(t, e.Lease.ClientID, got.Lease.ClientID)
	assert.True(t, e.Lease.IP.Equal(got.Lease.IP), "ip mismatch: %v vs %v", e.Lease.IP, got.Lease.IP)
	assert.Equal(t, e.Lease.SubnetMask, got.Lease.SubnetMask)
	assert.Equal(t, e.Lease.HWAddr.String(), got.Lease.HWAddr.String())
	assert.Equal(t, e.Lease.LeaseDuration, got.Lease.LeaseDuration)
	assert.Equal(t, e.Lease.T1, got.Lease.T1)
	assert.Equal(t, e.Lease.T2, got.Lease.T2)
	assert.True(t, e.Lease.AcquiredAt.Equal(got.Lease.AcquiredAt))
	assert.Equal(t, e.Lease.RawOffer, got.Lease.RawOffer)
	assert.Equal(t, e.Lease.RawACK, got.Lease.RawACK)
}

func TestStoreGetMissing(t *testing.T) {
	s := newTestStore(t, "az1")
	_, err := s.Get("absent")
	assert.True(t, errors.Is(err, nats.ErrKeyNotFound), "got %v", err)
}

func TestStoreDeleteIdempotent(t *testing.T) {
	s := newTestStore(t, "az1")
	require.NoError(t, s.Delete("never-existed"))

	require.NoError(t, s.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: sampleLease("x")}))
	require.NoError(t, s.Delete("x"))
	_, err := s.Get("x")
	assert.True(t, errors.Is(err, nats.ErrKeyNotFound))
}

func TestStoreListSkipsVersionKey(t *testing.T) {
	s := newTestStore(t, "az1")

	for _, id := range []string{"eipalloc-a", "eipalloc-b", "eipalloc-c"} {
		require.NoError(t, s.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: sampleLease(id)}))
	}

	entries, err := s.List()
	require.NoError(t, err)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Lease.ClientID] = true
	}
	assert.True(t, got["eipalloc-a"])
	assert.True(t, got["eipalloc-b"])
	assert.True(t, got["eipalloc-c"])
	assert.False(t, got["_version"], "version key must not appear in list")
}

func TestStorePutRejectsNilLease(t *testing.T) {
	s := newTestStore(t, "az1")
	err := s.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: nil})
	assert.Error(t, err)
}

func TestStorePutRejectsEmptyClientID(t *testing.T) {
	s := newTestStore(t, "az1")
	lease := sampleLease("")
	err := s.Put(dhcp.Entry{Purpose: "eip", PoolName: "default", Lease: lease})
	assert.Error(t, err)
}
