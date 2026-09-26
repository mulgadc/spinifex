package dhcp_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLeaseFor puts a live lease in the store under purpose and returns the
// address it holds, so a release can be aimed at it by IP the way vpcd is.
func seedLeaseFor(t *testing.T, fake *dhcp.Fake, store *dhcp.Store, purpose, clientID, ip string) string {
	t.Helper()
	hw, err := net.ParseMAC("02:00:00:aa:bb:cc")
	require.NoError(t, err)
	lease, err := fake.Acquire(context.Background(), dhcp.AcquireRequest{
		Bridge: "br-wan", ClientID: clientID, HWAddr: hw,
	})
	require.NoError(t, err)
	lease.IP = net.ParseIP(ip).To4()
	lease.LeaseDuration = time.Hour
	require.NoError(t, store.Put(t.Context(), dhcp.Entry{
		Purpose: purpose, PoolName: "wan", Lease: lease,
	}))
	return ip
}

// Instance teardown releases by address, and after an associate that address is
// the EIP. On a DHCP pool the lease is the only thing holding it — the server
// promises nothing about handing it back — so this release has to be refused.
func TestAnOwnerScopedReleaseLeavesAnEIPLeaseAlone(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, nc := newTestManager(t, "az1", fake, time.Now)
	ip := seedLeaseFor(t, fake, store, dhcp.PurposeEIP, "eipalloc-1", "192.0.2.50")

	_, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	client := dhcp.NewNATSClient(nc, 3*time.Second)

	require.NoError(t, client.RequestReleaseByIP(t.Context(), "wan", ip, true))

	assert.Zero(t, fake.ReleaseCount(),
		"terminating an instance sent DHCPRELEASE for a customer's Elastic IP")
	_, err = store.Get(t.Context(), "eipalloc-1")
	assert.NoError(t, err, "the EIP's lease was deleted, so nothing is renewing it")
}

// ReleaseAddress is the address's own owner and names no interface, so it must
// still be able to give the EIP up.
func TestAnUnscopedReleaseStillFreesAnEIPLease(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, nc := newTestManager(t, "az1", fake, time.Now)
	ip := seedLeaseFor(t, fake, store, dhcp.PurposeEIP, "eipalloc-1", "192.0.2.50")

	_, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	client := dhcp.NewNATSClient(nc, 3*time.Second)

	require.NoError(t, client.RequestReleaseByIP(t.Context(), "wan", ip, false))

	assert.Equal(t, 1, fake.ReleaseCount(), "ReleaseAddress could not free its own address")
}

// The refusal must not reach the auto-assigned address, which is exactly what
// instance teardown is there to reclaim.
func TestAnOwnerScopedReleaseStillFreesAnAutoAssignedLease(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, nc := newTestManager(t, "az1", fake, time.Now)
	ip := seedLeaseFor(t, fake, store, dhcp.PurposeENIPublic, "eni-1", "192.0.2.51")

	_, err := mgr.Subscribe(nc)
	require.NoError(t, err)
	client := dhcp.NewNATSClient(nc, 3*time.Second)

	require.NoError(t, client.RequestReleaseByIP(t.Context(), "wan", ip, true))

	assert.Equal(t, 1, fake.ReleaseCount(), "an auto-assigned lease leaked on teardown")
	_, err = store.Get(t.Context(), "eni-1")
	assert.Error(t, err, "the lease record outlived the address")
}
