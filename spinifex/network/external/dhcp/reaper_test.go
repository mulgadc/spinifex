package dhcp_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubOwner records what the reaper asked about and answers from a table.
type stubOwner struct {
	mu        sync.Mutex
	status    map[string]dhcp.OwnerStatus
	statusErr map[string]error
	discardEr map[string]error
	asked     []string
	discarded []string
}

func newStubOwner() *stubOwner {
	return &stubOwner{
		status:    map[string]dhcp.OwnerStatus{},
		statusErr: map[string]error{},
		discardEr: map[string]error{},
	}
}

func (s *stubOwner) Status(_ context.Context, e dhcp.Entry) (dhcp.OwnerStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, e.Lease.ClientID)
	if err := s.statusErr[e.Lease.ClientID]; err != nil {
		return dhcp.OwnerUnknown, err
	}
	return s.status[e.Lease.ClientID], nil
}

func (s *stubOwner) Discard(_ context.Context, e dhcp.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.discardEr[e.Lease.ClientID]; err != nil {
		return err
	}
	s.discarded = append(s.discarded, e.Lease.ClientID)
	return nil
}

func (s *stubOwner) discardedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.discarded...)
}

func TestReapOrphansReleasesLeaseWhoseOwnerIsGone(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-gone", dhcp.PurposeEIP, "")

	owner := newStubOwner()
	owner.status["eipalloc-gone"] = dhcp.OwnerGone
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, 1, fake.ReleaseCount(), "the address must go back upstream, not just out of KV")

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Equal(t, []string{"eipalloc-gone"}, owner.discardedIDs())
}

func TestReapOrphansKeepsLiveLease(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-live", dhcp.PurposeEIP, "")

	owner := newStubOwner()
	owner.status["eipalloc-live"] = dhcp.OwnerAlive
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)
	assert.Zero(t, fake.ReleaseCount())

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// A lookup that fails must read as "keep". Treating an unreachable daemon as
// "gone" would release every address in the pool at once.
func TestReapOrphansKeepsLeaseWhenOwnerLookupFails(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-unknown", dhcp.PurposeEIP, "")

	owner := newStubOwner()
	owner.statusErr["eipalloc-unknown"] = errors.New("no responders")
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)
	assert.Zero(t, fake.ReleaseCount())

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// An unknown verdict is the default, so a responder that answers with neither
// alive nor gone must not cost an address.
func TestReapOrphansKeepsLeaseOnUnknownStatus(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-maybe", dhcp.PurposeEIP, "")

	owner := newStubOwner()
	owner.status["eipalloc-maybe"] = dhcp.OwnerUnknown
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// Releasing an address while a router port still answers ARP for it invites a
// duplicate-IP conflict, so a failed teardown has to stop the release.
func TestReapOrphansKeepsLeaseWhenDiscardFails(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "dhcp-gw-lrp-vpc-1", dhcp.PurposeGatewayLRP, "vpc-1")

	owner := newStubOwner()
	owner.status["dhcp-gw-lrp-vpc-1"] = dhcp.OwnerGone
	owner.discardEr["dhcp-gw-lrp-vpc-1"] = errors.New("ovsdb unreachable")
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)
	assert.Zero(t, fake.ReleaseCount())

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestReapOrphansWithoutOwnerIsNoOp(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-1", dhcp.PurposeEIP, "")

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Zero(t, reaped)

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// One orphan among several must not stop the rest of the sweep.
func TestReapOrphansSweepsIndependently(t *testing.T) {
	fake := dhcp.NewFake()
	mgr, store, _ := newTestManager(t, "ap-southeast-2a", fake, nil)
	seedLiveLease(t, fake, store, "eipalloc-live", dhcp.PurposeEIP, "")
	seedLiveLease(t, fake, store, "eipalloc-gone", dhcp.PurposeEIP, "")
	seedLiveLease(t, fake, store, "eipalloc-unknown", dhcp.PurposeEIP, "")

	owner := newStubOwner()
	owner.status["eipalloc-live"] = dhcp.OwnerAlive
	owner.status["eipalloc-gone"] = dhcp.OwnerGone
	owner.statusErr["eipalloc-unknown"] = errors.New("timeout")
	mgr.SetLeaseOwner(owner)

	reaped, err := mgr.ReapOrphans(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	entries, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 2)
	held := []string{entries[0].Lease.ClientID, entries[1].Lease.ClientID}
	assert.ElementsMatch(t, []string{"eipalloc-live", "eipalloc-unknown"}, held)
}

func TestParseOwnerStatus(t *testing.T) {
	assert.Equal(t, dhcp.OwnerAlive, dhcp.ParseOwnerStatus(dhcp.OwnerStatusAlive))
	assert.Equal(t, dhcp.OwnerGone, dhcp.ParseOwnerStatus(dhcp.OwnerStatusGone))
	assert.Equal(t, dhcp.OwnerUnknown, dhcp.ParseOwnerStatus(dhcp.OwnerStatusUnknown))
	assert.Equal(t, dhcp.OwnerUnknown, dhcp.ParseOwnerStatus("something-newer"), "an unrecognised verdict must never read as gone")
	assert.Equal(t, "gone", dhcp.OwnerGone.String())
}
