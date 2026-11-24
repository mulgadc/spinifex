package kvsync_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const leaseKey = "test.lease"

// stopRenewal unlocks l when the test ends, so its renewal goroutine stops and
// the bubble can exit. It does nothing for a lease that was lost.
func stopRenewal(t *testing.T, l *kvsync.Lease, tok kvsync.Token) {
	t.Cleanup(func() { l.Unlock(context.Background(), tok) })
}

func TestLease_TakeOverOnlyAfterAFullTTLOfTheLastRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		fa := &faultyStore{KVStore: s[0]}
		a := kvsync.NewLease(fa, leaseKey, "node-a", testTTL)
		b := kvsync.NewLease(s[1], leaseKey, "node-b", testTTL)

		tokA, err := a.TryLock(t.Context())
		require.NoError(t, err)
		stopRenewal(t, a, tokA)

		// Several TTLs pass while a renews, and b never gets it.
		last := revision(t, s[0], leaseKey)
		for range 9 {
			advance(testTTL / 3)
			rev := revision(t, s[0], leaseKey)
			require.Greater(t, rev, last, "a renewal must rewrite the key")
			last = rev
			_, err := b.TryLock(t.Context())
			_, held := asHeld(err)
			require.True(t, held, "b took a lease that is being renewed: %v", err)
		}

		// a can no longer write; b has seen its last revision once.
		fa.down.Store(true)
		advance(testTTL - time.Nanosecond)
		_, err = b.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held, "b took over before a full TTL of the last revision: %v", err)

		advance(time.Nanosecond)
		tokB, err := b.TryLock(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "node-b", tokB.Holder)
		assert.Greater(t, tokB.Generation, tokA.Generation)
		b.Unlock(t.Context(), tokB)
	})
}

func TestLease_LateStarterWaitsAFullTTLFromItsOwnObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		fa := &faultyStore{KVStore: s[0]}
		a := kvsync.NewLease(fa, leaseKey, "node-a", testTTL)
		b := kvsync.NewLease(s[1], leaseKey, "node-b", testTTL)

		tokA, err := a.TryLock(t.Context())
		require.NoError(t, err)
		stopRenewal(t, a, tokA)
		advance(testTTL / 3)
		fa.down.Store(true)

		// b starts long after a's last renewal, and has no earlier observation.
		advance(10 * testTTL)
		_, err = b.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held, "b's first look must start its clock, not take over: %v", err)

		advance(testTTL - time.Nanosecond)
		_, err = b.TryLock(t.Context())
		_, held = asHeld(err)
		require.True(t, held, "b took over before a full TTL of its own observation: %v", err)

		advance(time.Nanosecond)
		tokB, err := b.TryLock(t.Context())
		require.NoError(t, err)
		b.Unlock(t.Context(), tokB)
	})
}

func TestLease_RestartedHolderAndFollowerRaceOnOneRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(3)
		// Each contender's write waits until both have read, so both CAS at one revision.
		gate := &barrier{pending: 2, all: make(chan struct{})}
		fDead := &faultyStore{KVStore: s[0]}
		dead := kvsync.NewLease(fDead, leaseKey, "node-a", testTTL)
		restarted := kvsync.NewLease(&gatedStore{KVStore: s[1], gate: gate}, leaseKey, "node-a", testTTL)
		follower := kvsync.NewLease(&gatedStore{KVStore: s[2], gate: gate}, leaseKey, "node-b", testTTL)

		old, err := dead.TryLock(t.Context())
		require.NoError(t, err)
		stopRenewal(t, dead, old)
		fDead.down.Store(true)
		rev := revision(t, s[0], leaseKey)

		// The follower's first look only observes, so it never reaches the gate.
		_, err = follower.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held)
		advance(testTTL)
		gate.ctx = waitCtx(t)

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
	})
}

func TestLease_ExcludesCallSitesInOneProcess(t *testing.T) {
	s := stores(t, 1)
	l := kvsync.NewLease(s[0], leaseKey, "node-a", testTTL)

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

func TestLease_StaleUnlockLeavesTheLaterAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		l := kvsync.NewLease(s[0], leaseKey, "node-a", testTTL)

		siteA, err := l.TryLock(t.Context())
		require.NoError(t, err)
		lostA := l.Lost()
		takeKey(t, s[1])
		advance(testTTL / 3)
		requireClosed(t, lostA, "site A's acquisition must be lost")

		// The key names another holder, so site B waits out a TTL before taking it.
		_, err = l.TryLock(t.Context())
		_, held := asHeld(err)
		require.True(t, held)
		advance(testTTL)
		siteB, err := l.TryLock(t.Context())
		require.NoError(t, err)

		l.Unlock(t.Context(), siteA)
		assert.True(t, l.Held(), "site A's stale unlock released site B's acquisition")
		assert.Equal(t, siteB.Generation, revision(t, s[0], leaseKey), "site A's stale unlock deleted site B's key")
		l.Unlock(t.Context(), siteB)
	})
}

// A Lease is reusable after Unlock and after a renewal loses the key, each
// acquisition with a fresh Lost and a higher generation.
func TestLease_MismatchClosesLostOnceAndStopsRenewing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		f := &faultyStore{KVStore: s[0]}
		l := kvsync.NewLease(f, leaseKey, "node-a", testTTL)

		first, err := l.TryLock(t.Context())
		require.NoError(t, err)
		firstLost := l.Lost()
		l.Unlock(t.Context(), first)
		requireClosed(t, firstLost, "Unlock must close the acquisition's Lost")

		second, err := l.TryLock(t.Context())
		require.NoError(t, err)
		lost := l.Lost()
		requireOpen(t, lost, "a new acquisition needs a fresh Lost channel")
		takeKey(t, s[1])
		advance(testTTL / 3)
		requireClosed(t, lost, "a renewal that lost its CAS must close Lost")
		require.False(t, l.Held())

		swaps := f.swaps.Load()
		advance(10 * testTTL)
		assert.Equal(t, swaps, f.swaps.Load(), "a lost lease must not renew again")

		// With the taker's key deleted the key is free, so the lease takes it at once.
		require.NoError(t, s[1].CompareAndDelete(t.Context(), leaseKey, revision(t, s[1], leaseKey)))
		third, err := l.TryLock(t.Context())
		require.NoError(t, err)
		requireOpen(t, l.Lost(), "a new acquisition needs a fresh Lost channel")
		assert.Greater(t, third.Generation, second.Generation)
		l.Unlock(t.Context(), third)
	})
}

func TestLease_UnavailableRenewalsLoseAtTwoThirdsOfTheTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		f := &faultyStore{KVStore: s[0]}
		l := kvsync.NewLease(f, leaseKey, "node-a", testTTL)

		gone, err := l.TryLock(t.Context())
		require.NoError(t, err)
		lost := l.Lost()
		advance(testTTL / 3) // lands, so the deadline counts from here

		f.down.Store(true)
		before := f.swaps.Load()
		advance(2*testTTL/3 - time.Nanosecond)
		requireOpen(t, lost, "Lost closed before 2*ttl/3 without a landed renewal")
		advance(time.Nanosecond)
		requireClosed(t, lost, "Lost must close once 2*ttl/3 has passed since the last landed renewal")
		assert.False(t, l.Held())
		assert.Greater(t, f.swaps.Load()-before, int64(1), "an unavailable renewal must be retried before the lease is given up")
		f.down.Store(false)

		readopted, err := l.TryLock(t.Context())
		require.NoError(t, err, "the surviving key names this holder, so it is adopted at once")
		assert.Greater(t, readopted.Generation, gone.Generation, "an adoption takes the generation of its own write")
		l.Unlock(t.Context(), gone)
		assert.Equal(t, readopted.Generation, revision(t, f, leaseKey), "the lost acquisition's unlock deleted the readopted key")
		l.Unlock(t.Context(), readopted)
	})
}

func TestLeaseGroup_SameLeaseUntilForget(t *testing.T) {
	s := stores(t, 1)
	g := kvsync.NewLeaseGroup(s[0], "eks.cluster.", "node-a", testTTL)

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
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		slow := &slowStore{KVStore: s[0]}
		a := kvsync.NewLease(slow, leaseKey, "node-a", testTTL)
		b := kvsync.NewLease(s[1], leaseKey, "node-b", testTTL)

		var tokB kvsync.Token
		slow.afterSwap = func(int64) {
			_, err := b.TryLock(t.Context())
			_, held := asHeld(err)
			require.True(t, held, "b must first observe a's committed write: %v", err)
			time.Sleep(testTTL)
			tokB, err = b.TryLock(t.Context())
			require.NoError(t, err, "b takes over after a full TTL of a's revision")
		}

		_, err := a.TryLock(t.Context())
		require.ErrorIs(t, err, kvsync.ErrUnavailable)
		assert.False(t, a.Held(), "a must not lead on a grant a peer has since taken over")
		assert.True(t, b.Held())
		b.Unlock(t.Context(), tokB)
	})
}

// A renewal counts from when it was sent, since a peer can have seen the
// revision that early, so the next renewal is due ttl/3 after that send.
func TestLease_DelayedRenewalCountsFromItsSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		slow := &slowStore{KVStore: s[0]}
		slow.afterSwap = func(n int64) {
			if n == 2 {
				time.Sleep(testTTL / 4)
			}
		}
		a := kvsync.NewLease(slow, leaseKey, "node-a", testTTL)
		tok, err := a.TryLock(t.Context())
		require.NoError(t, err)
		stopRenewal(t, a, tok)

		// The first renewal is sent at ttl/3 and answers at ttl/3 + ttl/4.
		advance(2*testTTL/3 - time.Nanosecond)
		require.EqualValues(t, 2, slow.swaps.Load(), "the second renewal came before ttl/3 after the first was sent")
		advance(time.Nanosecond)
		require.EqualValues(t, 3, slow.swaps.Load(), "the second renewal is due ttl/3 after the first was sent")
		requireOpen(t, a.Lost(), "a renewal that landed in time keeps the lease")
	})
}

// A grant that took most of its validity to answer is renewed at once,
// rather than ttl/3 after install, when its validity would already be gone.
func TestLease_SlowGrantIsRenewedFromItsSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		slow := &slowStore{KVStore: s[0]}
		slow.afterSwap = func(n int64) {
			if n == 1 {
				time.Sleep(testTTL / 2)
			}
		}
		a := kvsync.NewLease(slow, leaseKey, "node-a", testTTL)
		tok, err := a.TryLock(t.Context())
		require.NoError(t, err, "a grant inside its validity is installed")
		stopRenewal(t, a, tok)

		synctest.Wait()
		require.EqualValues(t, 2, slow.swaps.Load(), "a grant that answered at ttl/2 must be renewed at once")
		requireOpen(t, a.Lost(), "the prompt renewal keeps the lease")
	})
}

// A renewal stuck where its context cannot reach still loses the lease at the
// deadline, before a peer that waited a TTL can take over.
func TestLease_StuckRenewalLosesAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		slow := &slowStore{KVStore: s[0]}
		stuck := make(chan struct{})
		slow.afterSwap = func(n int64) {
			if n == 2 {
				<-stuck
			}
		}
		a := kvsync.NewLease(slow, leaseKey, "node-a", testTTL)
		tok, err := a.TryLock(t.Context())
		require.NoError(t, err)
		stopRenewal(t, a, tok)
		t.Cleanup(func() { close(stuck) })

		advance(2*testTTL/3 - time.Nanosecond)
		requireOpen(t, a.Lost(), "the lease was lost before 2*ttl/3 after the last send")
		advance(time.Nanosecond)
		requireClosed(t, a.Lost(), "the lease must be lost at 2*ttl/3 after the last send, stuck CAS or not")
		assert.False(t, a.Held())
	})
}

// Unlock does not wait out a renewal stuck in flight: it deletes the key at the
// last revision that landed, and the stuck write cannot bring the key back.
func TestLease_UnlockAbandonsAStuckRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		slow := &slowStore{KVStore: s[0]}
		entered, stuck := make(chan struct{}), make(chan struct{})
		unstick := sync.OnceFunc(func() { close(stuck) })
		t.Cleanup(unstick)
		slow.beforeSwap = func(n int64) {
			if n == 2 {
				close(entered)
				<-stuck
			}
		}
		a := kvsync.NewLease(slow, leaseKey, "node-a", testTTL)
		tok, err := a.TryLock(t.Context())
		require.NoError(t, err)

		advance(testTTL / 3)
		requireClosed(t, entered, "the renewal never started")

		unlocked := make(chan struct{})
		go func() { defer close(unlocked); a.Unlock(t.Context(), tok) }()
		synctest.Wait()
		requireClosed(t, unlocked, "Unlock waited for a renewal stuck in flight")
		_, _, err = s[0].Load(t.Context(), leaseKey)
		require.ErrorIs(t, err, kvsync.ErrNotFound, "Unlock must delete the key at the last landed revision")

		unstick()
		synctest.Wait()
		require.EqualValues(t, 2, slow.swaps.Load(), "the abandoned renewal must have been sent")
		_, _, err = s[0].Load(t.Context(), leaseKey)
		require.ErrorIs(t, err, kvsync.ErrNotFound, "the abandoned renewal must not recreate the key")
	})
}

func TestLease_ConstructionRefusesAnEmptyHolderOrANonPositiveTTL(t *testing.T) {
	s := stores(t, 1)
	assert.PanicsWithValue(t, "kvsync: empty holder", func() { kvsync.NewLease(s[0], "lease.noholder", "", testTTL) })
	assert.PanicsWithValue(t, "kvsync: lease ttl must be positive", func() { kvsync.NewLease(s[0], "lease.zerottl", "node-a", 0) })
	assert.PanicsWithValue(t, "kvsync: lease ttl must be positive", func() { kvsync.NewLease(s[0], "lease.negttl", "node-a", -time.Second) })
	assert.PanicsWithValue(t, "kvsync: empty holder", func() { kvsync.NewLeaseGroup(s[0], "group.noholder.", "", testTTL) })
	assert.PanicsWithValue(t, "kvsync: lease ttl must be positive", func() { kvsync.NewLeaseGroup(s[0], "group.zerottl.", "node-a", 0) })
	assert.PanicsWithValue(t, "kvsync: lease ttl must be positive", func() { kvsync.NewLeaseGroup(s[0], "group.negttl.", "node-a", -time.Second) })
}
