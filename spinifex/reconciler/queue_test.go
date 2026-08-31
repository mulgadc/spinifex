package reconciler_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyLog records which keys the per-key path was handed, in order, and lets a
// test wait for a count rather than sleep and hope.
type keyLog struct {
	mu   sync.Mutex
	keys []string
	// block, when set, holds each call until it is closed, so a test can make a
	// reconcile still be running when the next change arrives.
	block chan struct{}
}

func newKeyLog() *keyLog { return &keyLog{} }

func (l *keyLog) record(key string) {
	l.mu.Lock()
	block := l.block
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	if block != nil {
		<-block
	}
}

func (l *keyLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

func (l *keyLog) waitFor(t *testing.T, n int, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(l.seen()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d key reconciles (%s); saw %v", n, why, l.seen())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *keyLog) stillAt(t *testing.T, n int, why string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	assert.Len(t, l.seen(), n, why)
}

func TestRun_AnUpdateReconcilesTheKeyThatChanged(t *testing.T) {
	store, _ := newBucket(t, "perkey-one")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "one",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       time.Hour,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "one", &record{Name: "one"}))

	keys.waitFor(t, 1, "the key that changed")
	assert.Equal(t, []string{"one"}, keys.seen())
	p.stillAt(t, 1, "a per-key loop must not also run the whole set on a change")
}

// The burst case the whole-set path collapses into one pass becomes one pass
// per distinct key here, which is the entire point of the extension.
func TestRun_ABurstReconcilesEachKeyOnce(t *testing.T) {
	store, _ := newBucket(t, "perkey-burst")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "burst",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       time.Hour,
		Debounce:     200 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Set(t.Context(), key, &record{Name: key}))
	}

	keys.waitFor(t, 3, "one reconcile per key in the burst")
	got := keys.seen()
	sort.Strings(got)
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

// Repeated writes to one key inside a debounce window are one change to that
// key, not five: a fresh read makes the reconcile idempotent, so the four
// earlier revisions have nothing left to say.
func TestRun_RepeatedWritesToOneKeyReconcileItOnce(t *testing.T) {
	store, _ := newBucket(t, "perkey-dedup")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "dedup",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       time.Hour,
		Debounce:     200 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	for range 5 {
		require.NoError(t, store.Set(t.Context(), "hot", &record{Name: "hot"}))
	}

	keys.waitFor(t, 1, "the reconcile covering the burst")
	keys.stillAt(t, 1, "five writes to one key in one burst must reconcile it once")
}

// The case dedup must not swallow: a key taken off the queue is no longer
// pending, so a change arriving while it is being reconciled is a new piece of
// work rather than one the running pass is assumed to cover.
func TestRun_AChangeDuringAKeysReconcileIsNotLost(t *testing.T) {
	store, _ := newBucket(t, "perkey-inflight")
	p, keys := newPasses(), newKeyLog()
	keys.block = make(chan struct{})

	run(t, reconciler.Config{
		Name:         "inflight",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       time.Hour,
		Debounce:     50 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "hot", &record{Name: "first"}))
	keys.waitFor(t, 1, "the first reconcile, which is now blocked")

	// This write lands while the reconcile above is still in flight.
	require.NoError(t, store.Set(t.Context(), "hot", &record{Name: "second"}))
	time.Sleep(200 * time.Millisecond)
	close(keys.block)

	keys.waitFor(t, 2, "the second reconcile, for the change that arrived mid-pass")
}

// KeyFor exists for a caller whose unit of work is coarser than a key. Two
// instances in one account are one recompute, not two.
func TestRun_KeyForMapsUpdatesOntoTheCallersUnitOfWork(t *testing.T) {
	store, _ := newBucket(t, "perkey-keyfor")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "keyfor",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		KeyFor: func(entry jetstream.KeyValueEntry) ([]string, bool) {
			return []string{"account-1"}, true
		},
		Resync:   time.Hour,
		Debounce: 200 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	for _, key := range []string{"i-1", "i-2", "i-3"} {
		require.NoError(t, store.Set(t.Context(), key, &record{Name: key}))
	}

	keys.waitFor(t, 1, "the account the three instances belong to")
	keys.stillAt(t, 1, "three instances in one account are one unit of work")
	assert.Equal(t, []string{"account-1"}, keys.seen())
}

// An update that cannot be attributed — the delete case, where the work key
// lived in a value the tombstone does not carry — falls back to the pass that
// needs no attribution.
func TestRun_AnUnattributableUpdateFallsBackToTheWholeSet(t *testing.T) {
	store, _ := newBucket(t, "perkey-fallback")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "fallback",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		KeyFor: func(entry jetstream.KeyValueEntry) ([]string, bool) {
			if entry.Operation() == jetstream.KeyValuePut {
				return []string{entry.Key()}, true
			}
			return nil, false
		},
		Resync:   time.Hour,
		Debounce: 100 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "gone", &record{Name: "gone"}))
	keys.waitFor(t, 1, "the put, which is attributable")

	require.NoError(t, store.Delete(t.Context(), "gone"))
	p.waitFor(t, 2, "the whole-set pass covering an update that could not be attributed")
	keys.stillAt(t, 1, "the delete must not be reconciled as a key")
}

// The backstop is the reason ReconcileKey is an addition rather than a
// replacement: a key the queue never learned about — deleted while the watch
// was down, so nothing observed it — is still covered.
func TestRun_TheResyncStillRunsTheWholeSet(t *testing.T) {
	store, _ := newBucket(t, "perkey-resync")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "resync",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       150 * time.Millisecond,
	})

	p.waitFor(t, 4, "repeated whole-set resync passes with ReconcileKey set")
	assert.Empty(t, keys.seen(), "an idle bucket produces no per-key work")
}

func TestRun_AFailedKeyDoesNotStopTheLoop(t *testing.T) {
	store, _ := newBucket(t, "perkey-failure")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:      "failure",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) {
			keys.record(key)
			return 0, assert.AnError
		},
		Resync:   time.Hour,
		Debounce: 50 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "first", &record{Name: "first"}))
	keys.waitFor(t, 1, "the first key, which fails")
	require.NoError(t, store.Set(t.Context(), "second", &record{Name: "second"}))
	keys.waitFor(t, 2, "the next key, after the first one failed")
}
