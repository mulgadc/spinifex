package viperblock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock runs at wall speed from an offset a test can jump forward, which
// is how a reply that took tens of seconds is staged without waiting for it.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// slowAckKV applies each write on the server, then moves the clock on by the
// write's delay before acknowledging it. failUpdates refuses Updates unapplied,
// and gate, when set, holds an applied Update's reply until it is closed.
type slowAckKV struct {
	jetstream.KeyValue

	clock       *testClock
	createDelay time.Duration
	updateDelay time.Duration
	failUpdates bool
	gate        chan struct{}
	entered     chan struct{}

	mu      sync.Mutex
	applied []time.Time
}

func (k *slowAckKV) record() {
	k.mu.Lock()
	k.applied = append(k.applied, k.clock.now())
	k.mu.Unlock()
}

func (k *slowAckKV) lastApplied(t *testing.T) time.Time {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	require.NotEmpty(t, k.applied, "no write reached the server")
	return k.applied[len(k.applied)-1]
}

func (k *slowAckKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	if err == nil {
		k.record()
		k.clock.advance(k.createDelay)
	}
	return rev, err
}

func (k *slowAckKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if k.failUpdates {
		return 0, errors.New("store unwritable")
	}
	rev, err := k.KeyValue.Update(ctx, key, value, last)
	if err != nil {
		return rev, err
	}
	k.record()
	k.clock.advance(k.updateDelay)
	if k.gate != nil {
		close(k.entered)
		<-k.gate
	}
	return rev, nil
}

// newSlowAckLeases is a lease store whose writes go through a slowAckKV.
func newSlowAckLeases(t *testing.T, owner string) (*volumeLeases, *slowAckKV) {
	t.Helper()
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, owner)
	clock := &testClock{}
	kv := &slowAckKV{KeyValue: leases.kv, clock: clock}
	leases.kv = kv
	leases.now = clock.now
	return leases, kv
}

// stopRenewals ends the loop acquire started, so a test drives renewal itself.
func stopRenewals(lease *volumeLease) {
	lease.stop()
	<-lease.done
}

// TestVolumeLease_SlowAcquireAckDoesNotExtendValidity: the server's TTL starts
// when it applies the create, so validity measured from a late reply outlives
// the entry and lets a holder keep writing after a peer has taken the volume.
func TestVolumeLease_SlowAcquireAckDoesNotExtendValidity(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")
	kv.createDelay = 30 * time.Second
	kv.failUpdates = true // the generation publish never lands, so only the create proves the lease

	lease, err := leases.acquire(t.Context(), "vol-slowacquireack")
	require.NoError(t, err, "a lease acknowledged inside its validity is still usable")
	stopRenewals(lease)

	assert.False(t, lease.lastConfirmed().After(kv.lastApplied(t)),
		"validity counted from the reply rather than the send outlasts the server's entry by the reply's delay")
}

// TestVolumeLease_AcquireAckedAfterValidityIsRefused: a reply that took the
// whole validity window cannot authorize an open, because the server may
// already have expired the entry and granted it to a peer.
func TestVolumeLease_AcquireAckedAfterValidityIsRefused(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")
	kv.createDelay = volumeLeaseValidity + time.Second
	kv.failUpdates = true

	lease, err := leases.acquire(t.Context(), "vol-lateacquireack")
	require.Error(t, err)
	assert.Nil(t, lease)
	assert.ErrorIs(t, err, ErrLeaseStoreUnavailable, "the claim may succeed on a retry, so it must not read as held by a peer")

	leases.mu.Lock()
	defer leases.mu.Unlock()
	assert.NotContains(t, leases.held, "vol-lateacquireack", "a refused acquisition must not be shared with a later open")
}

// TestVolumeLease_GenerationPublishRenewsALateAcquisition: the publish is
// conditioned on the create's revision, so its success proves the entry is
// still ours as of when it was sent, however late the create's reply was.
func TestVolumeLease_GenerationPublishRenewsALateAcquisition(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")
	kv.createDelay = volumeLeaseValidity + time.Second

	lease, err := leases.acquire(t.Context(), "vol-latepublished")
	require.NoError(t, err)
	stopRenewals(lease)

	assert.False(t, lease.expiredLocally(), "a confirmed publish is a fresh proof of ownership")
	assert.False(t, lease.lastConfirmed().After(kv.lastApplied(t)),
		"validity must still start no later than the server applied the publish")
}

// TestVolumeLease_SlowRenewalAckDoesNotExtendValidity is the renewal half of
// the slow-reply overlap.
func TestVolumeLease_SlowRenewalAckDoesNotExtendValidity(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-slowrenewalack")
	require.NoError(t, err)
	stopRenewals(lease)

	kv.updateDelay = 10 * time.Second
	require.True(t, lease.renew(t.Context()))

	assert.False(t, lease.lastConfirmed().After(kv.lastApplied(t)),
		"validity counted from the reply rather than the send outlasts the server's entry by the reply's delay")
}

// TestVolumeLease_RenewalSucceedingAfterSurrenderDoesNotReviveTheLease: the
// export is already being fenced and a peer may hold the volume, so a renewal
// that lands afterwards must not make the holder valid again.
func TestVolumeLease_RenewalSucceedingAfterSurrenderDoesNotReviveTheLease(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-renewalaftersurrender")
	require.NoError(t, err)
	stopRenewals(lease)

	kv.gate = make(chan struct{})
	kv.entered = make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- lease.renew(t.Context()) }()
	<-kv.entered

	// The renewal is applied but unanswered; meanwhile validity runs out.
	lease.mu.Lock()
	lease.confirmed = leases.clock().Add(-volumeLeaseValidity - time.Second)
	lease.mu.Unlock()
	lease.surrender(t.Context())
	close(kv.gate)

	assert.False(t, <-result, "a renewal answered after surrender must not report the lease as ours")
	assert.True(t, lease.expiredLocally(), "a late renewal revived a lease whose export is being fenced")
	lease.mu.Lock()
	defer lease.mu.Unlock()
	assert.True(t, lease.lost)
}

// TestVolumeLease_LapseIsFencedAtTheDeadlineNotTheNextCheck: the check
// interval equals the margin, so waiting for the next tick to notice a lapse
// can reach the server's expiry and overlap the next holder.
func TestVolumeLease_LapseIsFencedAtTheDeadlineNotTheNextCheck(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	leases.checkEvery = time.Hour

	lost := make(chan leaseLossKind, 1)
	leases.onLost = func(_ context.Context, _ string, kind leaseLossKind) { lost <- kind }

	lease, err := leases.acquire(t.Context(), "vol-lapsedeadline")
	require.NoError(t, err)
	stopRenewals(lease)

	// Store unwritable, validity due to lapse shortly, and no tick before then.
	leases.kv = &slowAckKV{KeyValue: leases.kv, clock: &testClock{}, failUpdates: true}
	lease.mu.Lock()
	lease.confirmed = time.Now().Add(-volumeLeaseValidity + 100*time.Millisecond)
	lease.done = make(chan struct{})
	lease.mu.Unlock()
	go lease.renewLoop(t.Context())

	select {
	case kind := <-lost:
		assert.Equal(t, leaseLostStalled, kind)
	case <-time.After(5 * time.Second):
		t.Fatal("validity lapsed and nothing fenced the volume until the next check")
	}
}

// TestVolumeLease_LostOrLapsedLeaseIsNotShared: a lease stays held until its
// fence releases it, and handing it to another open in between would
// authorize writes the lease no longer covers.
func TestVolumeLease_LostOrLapsedLeaseIsNotShared(t *testing.T) {
	t.Run("lost", func(t *testing.T) {
		leases, _ := newSlowAckLeases(t, "node-a")
		lease, err := leases.acquire(t.Context(), "vol-sharelost")
		require.NoError(t, err)
		stopRenewals(lease)
		lease.surrender(t.Context())

		shared, err := leases.acquire(t.Context(), "vol-sharelost")
		require.ErrorIs(t, err, ErrLeaseStoreUnavailable)
		assert.Nil(t, shared)
		lease.mu.Lock()
		defer lease.mu.Unlock()
		assert.Equal(t, 1, lease.refs)
	})
	t.Run("lapsed", func(t *testing.T) {
		leases, kv := newSlowAckLeases(t, "node-a")
		lease, err := leases.acquire(t.Context(), "vol-sharelapsed")
		require.NoError(t, err)
		stopRenewals(lease)
		kv.clock.advance(volumeLeaseValidity + time.Second)

		shared, err := leases.acquire(t.Context(), "vol-sharelapsed")
		require.ErrorIs(t, err, ErrLeaseStoreUnavailable)
		assert.Nil(t, shared)
		lease.mu.Lock()
		defer lease.mu.Unlock()
		assert.Equal(t, 1, lease.refs)
	})
}

// TestVolumeLease_RenewalAckedAfterTheDeadlineDoesNotRevive: the renewal was
// sent inside validity, but its reply lands after the deadline and before the
// deadline timer has run. The holder must give the volume up rather than let
// the new confirmation hide the lapse from the timer.
func TestVolumeLease_RenewalAckedAfterTheDeadlineDoesNotRevive(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")
	var fences []leaseLossKind
	var fenceMu sync.Mutex
	fenced := make(chan struct{}, 4)
	leases.onLost = func(_ context.Context, _ string, kind leaseLossKind) {
		fenceMu.Lock()
		fences = append(fences, kind)
		fenceMu.Unlock()
		fenced <- struct{}{}
	}

	lease, err := leases.acquire(t.Context(), "vol-renewalpastdeadline")
	require.NoError(t, err)
	stopRenewals(lease)

	// One second of validity left when the renewal is sent.
	lease.mu.Lock()
	lease.confirmed = leases.clock().Add(-volumeLeaseValidity + time.Second)
	lease.mu.Unlock()

	kv.gate = make(chan struct{})
	kv.entered = make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- lease.renew(t.Context()) }()
	<-kv.entered
	kv.clock.advance(2 * time.Second) // the old deadline passes while the reply is held
	close(kv.gate)

	assert.False(t, <-result, "a renewal answered after the deadline must not keep the lease")
	assert.True(t, lease.expiredLocally(), "a reply after the deadline revived the lease")
	select {
	case <-fenced:
	case <-time.After(5 * time.Second):
		t.Fatal("a renewal answered after the deadline left the volume unfenced")
	}

	// The deadline timer and a peer conflict arriving afterwards fence nothing more.
	lease.surrender(t.Context())
	lease.loseToPeer(t.Context(), errors.New("conflict"), "late")
	select {
	case <-fenced:
		t.Fatal("a lease already given up was fenced a second time")
	case <-time.After(200 * time.Millisecond):
	}
	fenceMu.Lock()
	defer fenceMu.Unlock()
	assert.Equal(t, []leaseLossKind{leaseLostStalled}, fences)
}

// TestVolumeLease_RenewalAckedJustBeforeTheDeadlineKeepsTheLease is the
// boundary: a reply that lands while validity still holds is a real renewal.
func TestVolumeLease_RenewalAckedJustBeforeTheDeadlineKeepsTheLease(t *testing.T) {
	leases, kv := newSlowAckLeases(t, "node-a")
	lease, err := leases.acquire(t.Context(), "vol-renewalbeforedeadline")
	require.NoError(t, err)
	stopRenewals(lease)

	lease.mu.Lock()
	lease.confirmed = leases.clock().Add(-volumeLeaseValidity + 2*time.Second)
	lease.mu.Unlock()
	kv.updateDelay = time.Second

	require.True(t, lease.renew(t.Context()))
	assert.False(t, lease.expiredLocally())
}
