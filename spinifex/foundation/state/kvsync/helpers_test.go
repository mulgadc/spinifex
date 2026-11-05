package kvsync_test

import (
	"context"
	"errors"
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
	g.gate.wait()
	return g.KVStore.CompareAndSwap(ctx, key, value, rev)
}

type barrier struct {
	wg sync.WaitGroup
}

func newBarrier(n int) *barrier {
	b := &barrier{}
	b.wg.Add(n)
	return b
}

func (b *barrier) wait() {
	b.wg.Done()
	b.wg.Wait()
}

func asHeld(err error) (*kvsync.HeldError, bool) {
	return errors.AsType[*kvsync.HeldError](err)
}

// slowStore wraps a store so that a CompareAndSwap which has committed can run
// afterSwap before its response returns, standing in for a delayed reply.
type slowStore struct {
	kvsync.KVStore

	swaps     atomic.Int64
	afterSwap func(n int64)
}

func (s *slowStore) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	next, err := s.KVStore.CompareAndSwap(ctx, key, value, rev)
	if n := s.swaps.Add(1); err == nil && s.afterSwap != nil {
		s.afterSwap(n)
	}
	return next, err
}
