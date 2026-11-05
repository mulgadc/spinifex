package kvsync_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const leaseKey = "test.lease"

// stopRenewal unlocks a lease whose clock was frozen to stand in for a dead
// process, so its renewal goroutine ends with the test.
func stopRenewal(t *testing.T, l *kvsync.Lease, tok kvsync.Token) {
	t.Cleanup(func() { l.Unlock(context.Background(), tok) })
}

func TestLease_TakeOverOnlyAfterAFullTTLOfTheLastRevision(t *testing.T) {
	s := stores(t, 2)
	ca, cb := kvsync.NewFakeClock(), kvsync.NewFakeClock()
	a := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, ca)
	b := kvsync.NewLeaseWithClock(s[1], leaseKey, "node-b", testTTL, cb)

	tokA, err := a.TryLock(t.Context())
	require.NoError(t, err)
	stopRenewal(t, a, tokA)

	// Several TTLs pass on both clocks while a renews, and b never gets it.
	last := revision(t, s[0], leaseKey)
	for range 9 {
		renewOnce(t, ca)
		rev := revision(t, s[0], leaseKey)
		require.Greater(t, rev, last, "a renewal must rewrite the key")
		last = rev
		cb.Advance(testTTL / 3)
		_, err := b.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held, "b took a lease that is being renewed: %v", err)
	}

	// a stops renewing without unlocking; b has seen its last revision once.
	cb.Advance(testTTL - time.Nanosecond)
	_, err = b.TryLock(t.Context())
	_, held := asHeld(err)
	require.True(t, held, "b took over before a full TTL of the last revision: %v", err)

	cb.Advance(time.Nanosecond)
	tokB, err := b.TryLock(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "node-b", tokB.Holder)
	assert.Greater(t, tokB.Generation, tokA.Generation)
	b.Unlock(t.Context(), tokB)
}

func TestLease_LateStarterWaitsAFullTTLFromItsOwnObservation(t *testing.T) {
	s := stores(t, 2)
	ca, cb := kvsync.NewFakeClock(), kvsync.NewFakeClock()
	a := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, ca)
	b := kvsync.NewLeaseWithClock(s[1], leaseKey, "node-b", testTTL, cb)

	tokA, err := a.TryLock(t.Context())
	require.NoError(t, err)
	stopRenewal(t, a, tokA)
	renewOnce(t, ca)

	// b starts long after a's last renewal, and has no earlier observation.
	cb.Advance(10 * testTTL)
	_, err = b.TryLock(t.Context())
	_, held := asHeld(err)
	require.True(t, held, "b's first look must start its clock, not take over: %v", err)

	cb.Advance(testTTL - time.Nanosecond)
	_, err = b.TryLock(t.Context())
	_, held = asHeld(err)
	require.True(t, held, "b took over before a full TTL of its own observation: %v", err)

	cb.Advance(time.Nanosecond)
	tokB, err := b.TryLock(t.Context())
	require.NoError(t, err)
	b.Unlock(t.Context(), tokB)
}

func TestLease_RestartedHolderAndFollowerRaceOnOneRevision(t *testing.T) {
	s := stores(t, 3)
	// Each contender's write waits until both have read, so both CAS at one revision.
	gate := newBarrier(2)
	cDead, cNew, cFollow := kvsync.NewFakeClock(), kvsync.NewFakeClock(), kvsync.NewFakeClock()
	dead := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, cDead)
	restarted := kvsync.NewLeaseWithClock(&gatedStore{KVStore: s[1], gate: gate}, leaseKey, "node-a", testTTL, cNew)
	follower := kvsync.NewLeaseWithClock(&gatedStore{KVStore: s[2], gate: gate}, leaseKey, "node-b", testTTL, cFollow)

	old, err := dead.TryLock(t.Context())
	require.NoError(t, err)
	stopRenewal(t, dead, old)
	rev := revision(t, s[0], leaseKey)

	// The follower's first look only observes, so it never reaches the gate.
	_, err = follower.TryLock(t.Context())
	_, held := asHeld(err)
	require.True(t, held)
	cFollow.Advance(testTTL)

	type result struct {
		tok kvsync.Token
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, l := range []*kvsync.Lease{restarted, follower} {
		wg.Go(func() {
			tok, err := l.TryLock(t.Context())
			results[i] = result{tok, err}
		})
	}
	wg.Wait()

	var winners int
	for _, r := range results {
		if r.err == nil {
			winners++
			assert.Greater(t, r.tok.Generation, rev)
			continue
		}
		_, held := asHeld(r.err)
		assert.True(t, held, "the loser must get HeldError, got %v", r.err)
	}
	require.Equal(t, 1, winners, "exactly one CAS at the shared revision wins")
	for i, l := range []*kvsync.Lease{restarted, follower} {
		if results[i].err == nil {
			l.Unlock(t.Context(), results[i].tok)
		}
	}
}

func TestLease_ExcludesCallSitesInOneProcess(t *testing.T) {
	s := stores(t, 1)
	l := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, kvsync.NewFakeClock())

	siteA, err := l.TryLock(t.Context())
	require.NoError(t, err)

	_, err = l.TryLock(t.Context())
	he, held := asHeld(err)
	require.True(t, held, "a second site took a held Lease: %v", err)
	assert.Equal(t, siteA, he.Current, "the refusal names this holder's acquisition")

	l.Unlock(t.Context(), siteA)
	siteB, err := l.TryLock(t.Context())
	require.NoError(t, err)
	assert.Greater(t, siteB.Generation, siteA.Generation)
	l.Unlock(t.Context(), siteB)
}

// takeKey overwrites the key from another store, standing in for a holder
// that took the lease over.
func takeKey(t *testing.T, other kvsync.KVStore) {
	t.Helper()
	_, rev, err := other.Load(t.Context(), leaseKey)
	require.NoError(t, err)
	_, err = other.CompareAndSwap(t.Context(), leaseKey, []byte(`{"holder":"node-b"}`), rev)
	require.NoError(t, err)
}

func TestLease_ReusableAfterUnlockAndAfterLoss(t *testing.T) {
	s := stores(t, 2)
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, c)

	first, err := l.TryLock(t.Context())
	require.NoError(t, err)
	firstLost := l.Lost()
	l.Unlock(t.Context(), first)
	requireClosed(t, firstLost, "Unlock must close the acquisition's Lost")

	second, err := l.TryLock(t.Context())
	require.NoError(t, err)
	secondLost := l.Lost()
	requireOpen(t, secondLost, "a new acquisition needs a fresh Lost channel")

	takeKey(t, s[1])
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	requireClosed(t, secondLost, "a renewal that lost its CAS must close Lost")
	require.False(t, l.Held())

	// The taker's key is adopted only after a TTL, as any other holder's.
	require.NoError(t, s[1].CompareAndDelete(t.Context(), leaseKey, revision(t, s[1], leaseKey)))
	third, err := l.TryLock(t.Context())
	require.NoError(t, err)
	requireOpen(t, l.Lost(), "a new acquisition needs a fresh Lost channel")
	assert.Greater(t, third.Generation, second.Generation)
	l.Unlock(t.Context(), third)
}

func TestLease_StaleUnlockLeavesTheLaterAcquisition(t *testing.T) {
	s := stores(t, 2)
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, c)

	siteA, err := l.TryLock(t.Context())
	require.NoError(t, err)
	lostA := l.Lost()
	takeKey(t, s[1])
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	requireClosed(t, lostA, "site A's acquisition must be lost")

	// The key names another holder, so site B waits out a TTL before taking it.
	_, err = l.TryLock(t.Context())
	_, held := asHeld(err)
	require.True(t, held)
	c.Advance(testTTL)
	siteB, err := l.TryLock(t.Context())
	require.NoError(t, err)

	l.Unlock(t.Context(), siteA)
	assert.True(t, l.Held(), "site A's stale unlock released site B's acquisition")
	cur, ok, err := l.Holder(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "site A's stale unlock deleted site B's key")
	assert.Equal(t, siteB, cur)
	l.Unlock(t.Context(), siteB)
}

func TestLease_ReadoptionAfterUnavailableLossTakesANewGeneration(t *testing.T) {
	s := stores(t, 1)
	f := &faultyStore{KVStore: s[0]}
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(f, leaseKey, "node-a", testTTL, c)

	lost, err := l.TryLock(t.Context())
	require.NoError(t, err)
	lostCh := l.Lost()

	f.down.Store(true)
	failRenewalsUntilDeadline(t, c, lostCh)
	requireClosed(t, lostCh, "renewals unavailable for 2*ttl/3 must lose the lease")
	f.down.Store(false)

	readopted, err := l.TryLock(t.Context())
	require.NoError(t, err, "the surviving key names this holder, so it is adopted at once")
	assert.Greater(t, readopted.Generation, lost.Generation, "an adoption takes the generation of its own write")

	l.Unlock(t.Context(), lost)
	cur, ok, err := l.Holder(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "the lost acquisition's unlock deleted the readopted key")
	assert.Equal(t, readopted, cur)
	l.Unlock(t.Context(), readopted)
}

func TestLease_MismatchClosesLostOnceAndStopsRenewing(t *testing.T) {
	s := stores(t, 2)
	f := &faultyStore{KVStore: s[0]}
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(f, leaseKey, "node-a", testTTL, c)

	_, err := l.TryLock(t.Context())
	require.NoError(t, err)
	lost := l.Lost()
	takeKey(t, s[1])
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	requireClosed(t, lost, "a renewal that lost its CAS must close Lost")

	swaps := f.swaps.Load()
	c.Advance(10 * testTTL)
	assert.Zero(t, c.Waiters(), "a lost lease must not schedule another renewal")
	assert.Equal(t, swaps, f.swaps.Load(), "a lost lease must not renew again")
}

// failRenewalsUntilDeadline drives a lease whose last renewal landed now
// through failing renewals up to 2*ttl/3, checking Lost stays open before it.
// The first attempt comes at ttl/3 and each retry ttl/12 after the last.
func failRenewalsUntilDeadline(t *testing.T, c *kvsync.FakeClock, lost <-chan struct{}) {
	t.Helper()
	step := testTTL / 12
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	for elapsed := testTTL / 3; elapsed < 2*testTTL/3; elapsed += step {
		waitForWaiters(t, c, 1)
		requireOpen(t, lost, "Lost closed before 2*ttl/3 without a landed renewal")
		c.Advance(step)
	}
}

func TestLease_UnavailableRenewalsLoseAtTwoThirdsOfTheTTL(t *testing.T) {
	s := stores(t, 1)
	f := &faultyStore{KVStore: s[0]}
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(f, leaseKey, "node-a", testTTL, c)

	_, err := l.TryLock(t.Context())
	require.NoError(t, err)
	lost := l.Lost()
	renewOnce(t, c) // lands, so the deadline counts from here

	f.down.Store(true)
	before := f.swaps.Load()
	failRenewalsUntilDeadline(t, c, lost)
	requireClosed(t, lost, "Lost must close once 2*ttl/3 has passed since the last landed renewal")
	assert.False(t, l.Held())
	assert.Greater(t, f.swaps.Load()-before, int64(1), "an unavailable renewal must be retried before the lease is given up")
}

func TestLeaseGroup_SameLeaseUntilForget(t *testing.T) {
	s := stores(t, 1)
	g := kvsync.NewLeaseGroup(s[0], "eks.cluster.", "node-a", testTTL)
	kvsync.SetGroupClock(g, kvsync.NewFakeClock())

	l := g.Get("123/alpha")
	require.Same(t, l, g.Get("123/alpha"), "Get must return the one Lease a name has")
	assert.Equal(t, "eks.cluster.123/alpha", l.Key())

	tok, err := l.TryLock(t.Context())
	require.NoError(t, err)
	g.Forget("123/alpha")
	assert.False(t, l.Held(), "Forget unlocks the current acquisition")
	assert.Zero(t, revision(t, s[0], "eks.cluster.123/alpha"), "Forget deletes the key")

	fresh := g.Get("123/alpha")
	require.NotSame(t, l, fresh, "a forgotten name starts from a fresh Lease")
	again, err := fresh.TryLock(t.Context())
	require.NoError(t, err)
	assert.Greater(t, again.Generation, tok.Generation)
	g.Forget("123/alpha")
}

// A grant whose reply comes back after a peer has already waited out a TTL and
// taken over must not be reported as held: only one node may think it leads.
func TestLease_DelayedGrantIsRefusedAfterATakeover(t *testing.T) {
	s := stores(t, 2)
	ca, cb := kvsync.NewFakeClock(), kvsync.NewFakeClock()
	slow := &slowStore{KVStore: s[0]}
	a := kvsync.NewLeaseWithClock(slow, leaseKey, "node-a", testTTL, ca)
	b := kvsync.NewLeaseWithClock(s[1], leaseKey, "node-b", testTTL, cb)

	var tokB kvsync.Token
	slow.afterSwap = func(int64) {
		_, err := b.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held, "b must first observe a's committed write: %v", err)
		cb.Advance(testTTL)
		tokB, err = b.TryLock(t.Context())
		require.NoError(t, err, "b takes over after a full TTL of a's revision")
		ca.Advance(testTTL)
	}

	_, err := a.TryLock(t.Context())
	require.ErrorIs(t, err, kvsync.ErrUnavailable)
	assert.False(t, a.Held(), "a must not lead on a grant a peer has since taken over")
	assert.True(t, b.Held())
	b.Unlock(t.Context(), tokB)
}

// A renewal's validity runs from when it was sent, since a peer can have seen
// the revision that early, so a reply delayed by 2*ttl/3 leaves nothing to renew on.
func TestLease_DelayedRenewalCountsFromItsSend(t *testing.T) {
	s := stores(t, 1)
	c := kvsync.NewFakeClock()
	slow := &slowStore{KVStore: s[0]}
	a := kvsync.NewLeaseWithClock(slow, leaseKey, "node-a", testTTL, c)
	tok, err := a.TryLock(t.Context())
	require.NoError(t, err)
	stopRenewal(t, a, tok)

	slow.afterSwap = func(n int64) {
		if n == 2 {
			c.Advance(2 * testTTL / 3)
		}
	}
	renewOnce(t, c)
	requireOpen(t, a.Lost(), "the delayed renewal itself landed")

	c.Advance(testTTL / 3)
	requireClosed(t, a.Lost(), "the next renewal had no validity left from the delayed one's send")
}
