package kvsync_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/require"
)

// testTTL is a lease's ttl in the synctest tests, where only its ratio to
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

// advance moves the bubble's clock on by d, then waits until every goroutine
// that d woke, such as a renewal, has finished and blocked again.
func advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func requireClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	default:
		require.FailNow(t, msg)
	}
}

func requireOpen(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
		require.FailNow(t, msg)
	default:
	}
}

// memBucket is an in-memory sync bucket for the lease timing tests, which run
// in a synctest bubble that a JetStream server's network I/O would leave.
type memBucket struct {
	mu   sync.Mutex
	rev  uint64
	keys map[string]memEntry
}

type memEntry struct {
	value []byte
	rev   uint64
}

// memStore is one process's handle on a memBucket.
type memStore struct{ b *memBucket }

// memStores returns n distinct store values over one memBucket.
func memStores(n int) []*memStore {
	b := &memBucket{keys: map[string]memEntry{}}
	out := make([]*memStore, n)
	for i := range out {
		out[i] = &memStore{b: b}
	}
	return out
}

func (m *memStore) Load(_ context.Context, key string) ([]byte, uint64, error) {
	m.b.mu.Lock()
	defer m.b.mu.Unlock()
	e, ok := m.b.keys[key]
	if !ok {
		return nil, 0, kvsync.ErrNotFound
	}
	return e.value, e.rev, nil
}

func (m *memStore) CompareAndSwap(_ context.Context, key string, value []byte, rev uint64) (uint64, error) {
	m.b.mu.Lock()
	defer m.b.mu.Unlock()
	if m.b.keys[key].rev != rev {
		return 0, kvsync.ErrRevisionMismatch
	}
	m.b.rev++
	m.b.keys[key] = memEntry{value: value, rev: m.b.rev}
	return m.b.rev, nil
}

func (m *memStore) CompareAndDelete(_ context.Context, key string, rev uint64) error {
	m.b.mu.Lock()
	defer m.b.mu.Unlock()
	if e, ok := m.b.keys[key]; !ok || e.rev != rev {
		return kvsync.ErrRevisionMismatch
	}
	delete(m.b.keys, key)
	return nil
}

var _ kvsync.KVStore = (*memStore)(nil)

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
