package kvsync_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/require"
)

// testTTL is a lease's ttl in the fake-clock tests, where only its ratio to
// the renewal and retry intervals matters.
const testTTL = 60 * time.Second

// stores returns n distinct store values over one sync bucket, each standing
// in for a separate process.
func stores(t *testing.T, n int) []*kvstore.Bucket {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	out := make([]*kvstore.Bucket, n)
	for i := range out {
		out[i] = kvstore.OpenSyncBucket(js)
	}
	return out
}

// revision is the key's current revision, or 0 when it is absent.
func revision(t *testing.T, s kvsync.KVStore, key string) uint64 {
	t.Helper()
	_, rev, err := s.Load(t.Context(), key)
	if err != nil {
		require.ErrorIs(t, err, kvsync.ErrNotFound)
		return 0
	}
	return rev
}

// waitCtx bounds a wait on a condition so a broken test fails rather than
// hanging; it is never what makes the condition true.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitForWaiters fails the test unless c has n pending waiters in time.
func waitForWaiters(t *testing.T, c *kvsync.FakeClock, n int) {
	t.Helper()
	require.True(t, c.WaitForWaiters(waitCtx(t), n), "clock never reached %d waiters", n)
}

// renewOnce advances c by one renewal interval and waits for the renewal to
// finish and schedule the next.
func renewOnce(t *testing.T, c *kvsync.FakeClock) {
	t.Helper()
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	waitForWaiters(t, c, 1)
}

func requireClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-waitCtx(t).Done():
		t.Fatal(msg)
	}
}

func requireOpen(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(msg)
	default:
	}
}

// faultyStore wraps a store, failing writes with ErrUnavailable while down is
// set and counting every CompareAndSwap.
type faultyStore struct {
	kvsync.KVStore

	down  atomic.Bool
	swaps atomic.Int64
}

func (f *faultyStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	f.swaps.Add(1)
	if f.down.Load() {
		return 0, kvsync.ErrUnavailable
	}
	return f.KVStore.CompareAndSwap(ctx, key, value, rev)
}

// gatedStore wraps a store so that its CompareAndSwap waits at a shared
// barrier, holding contenders until every one has read the same revision.
type gatedStore struct {
	kvsync.KVStore

	gate *barrier
}

func (g *gatedStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	if err := g.gate.wait(); err != nil {
		return 0, err
	}
	return g.KVStore.CompareAndSwap(ctx, key, value, rev)
}

// barrier releases its callers once n have arrived, and fails each of them
// instead when ctx ends first, so a contender that never arrives fails the test.
type barrier struct {
	ctx     context.Context
	mu      sync.Mutex
	pending int
	all     chan struct{}
}

func newBarrier(ctx context.Context, n int) *barrier {
	return &barrier{ctx: ctx, pending: n, all: make(chan struct{})}
}

func (b *barrier) wait() error {
	b.mu.Lock()
	b.pending--
	if b.pending == 0 {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
		return nil
	case <-b.ctx.Done():
		return fmt.Errorf("barrier: not every contender arrived: %w", b.ctx.Err())
	}
}

// racingStore runs race once, before the first CompareAndSwap, standing in for
// a peer whose write lands between this caller's read and its write.
type racingStore struct {
	kvsync.KVStore

	race func()
	once sync.Once
}

func (r *racingStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	r.once.Do(r.race)
	return r.KVStore.CompareAndSwap(ctx, key, value, rev)
}

func asHeld(err error) (*kvsync.HeldError, bool) {
	return errors.AsType[*kvsync.HeldError](err)
}

// slowStore wraps a store so that a CompareAndSwap can run beforeSwap before it
// is sent, standing in for a stalled request, and afterSwap once it has
// committed but before its response returns, standing in for a delayed reply.
type slowStore struct {
	kvsync.KVStore

	calls      atomic.Int64
	swaps      atomic.Int64
	beforeSwap func(n int64)
	afterSwap  func(n int64)
}

func (s *slowStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	if n := s.calls.Add(1); s.beforeSwap != nil {
		s.beforeSwap(n)
	}
	next, err := s.KVStore.CompareAndSwap(ctx, key, value, rev)
	if n := s.swaps.Add(1); err == nil && s.afterSwap != nil {
		s.afterSwap(n)
	}
	return next, err
}

// waitForSwaps fails the test unless s has seen n CompareAndSwap calls in time.
func waitForSwaps(t *testing.T, s *slowStore, n int64) {
	t.Helper()
	ctx := waitCtx(t)
	for s.swaps.Load() < n {
		select {
		case <-ctx.Done():
			t.Fatalf("only %d of %d swaps happened", s.swaps.Load(), n)
		case <-time.After(time.Millisecond):
		}
	}
}

// staleStore answers its first Load with value at rev, as a replica behind the
// leader would, and every later Load from the store itself.
type staleStore struct {
	kvsync.KVStore

	value []byte
	rev   uint64
	used  atomic.Bool
}

func (s *staleStore) Load(ctx context.Context, key string) ([]byte, uint64, error) {
	if s.used.CompareAndSwap(false, true) {
		return s.value, s.rev, nil
	}
	return s.KVStore.Load(ctx, key)
}
