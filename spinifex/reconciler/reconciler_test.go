package reconciler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// record is a stand-in for whatever the watched bucket holds; the loop never
// decodes it, only notices that it changed.
type record struct {
	Name string `json:"name"`
}

// passes counts reconcile calls and lets a test wait for the count to reach a
// value, so no test sleeps for a fixed duration hoping a pass happened.
type passes struct {
	mu    sync.Mutex
	cond  *sync.Cond
	count int
}

func newPasses() *passes {
	p := &passes{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *passes) record() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	p.cond.Broadcast()
}

func (p *passes) now() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// waitFor blocks until at least n passes have run, failing the test rather than
// hanging if they do not.
func (p *passes) waitFor(t *testing.T, n int, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.count < n {
		p.mu.Unlock()
		if time.Now().After(deadline) {
			p.mu.Lock()
			t.Fatalf("timed out waiting for %d passes (%s); saw %d", n, why, p.count)
		}
		time.Sleep(5 * time.Millisecond)
		p.mu.Lock()
	}
}

// stillAt asserts no further pass ran during a short settling window, for the
// tests whose point is that something did NOT trigger a pass.
func (p *passes) stillAt(t *testing.T, n int, why string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, n, p.now(), why)
}

// newBucket starts an embedded JetStream server and returns a store over a
// bucket on it, plus the connection so a test can drop it.
func newBucket(t *testing.T, name string) (*kvstore.Store[record], *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return kvstore.New[record](testutil.NewJetStream(t, nc), kvstore.Config{
		Name:    name,
		History: 1,
	}), nc
}

// run starts the loop under a context the test cancels on cleanup.
func run(t *testing.T, cfg reconciler.Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconciler.Run(ctx, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestRun_ReconcilesOnceAtStartup(t *testing.T) {
	store, _ := newBucket(t, "reconciler-startup")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "startup",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    time.Hour,
	})

	p.waitFor(t, 1, "the startup pass")
	p.stillAt(t, 1, "an idle bucket must not produce further passes")
}

func TestRun_AnUpdateTriggersAPass(t *testing.T) {
	store, _ := newBucket(t, "reconciler-update")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "update",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    time.Hour,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "one", &record{Name: "one"}))
	p.waitFor(t, 2, "the pass triggered by the write")
}

// TestRun_ABurstCollapsesIntoOnePass is the whole point of the debounce: a
// multi-key write is one logical change, and a full-recompute reconcile gains
// nothing from running once per key.
func TestRun_ABurstCollapsesIntoOnePass(t *testing.T) {
	store, _ := newBucket(t, "reconciler-burst")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "burst",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    time.Hour,
		Debounce:  200 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		require.NoError(t, store.Set(t.Context(), key, &record{Name: key}))
	}

	p.waitFor(t, 2, "the pass covering the burst")
	p.stillAt(t, 2, "eight writes in one burst must produce one pass, not eight")
}

func TestRun_ResyncFiresWithNoWatchTraffic(t *testing.T) {
	store, _ := newBucket(t, "reconciler-resync")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "resync",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    150 * time.Millisecond,
	})

	p.waitFor(t, 4, "repeated resync passes against a bucket nobody is writing to")
}

// TestRun_NoSourcesStillReconcilesOnTheResync pins the degenerate case: a loop
// with nothing watchable is the pre-watch behaviour, not a dead loop.
func TestRun_NoSourcesStillReconcilesOnTheResync(t *testing.T) {
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "no-sources",
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    150 * time.Millisecond,
	})

	p.waitFor(t, 3, "resync passes with no sources configured")
}

// TestRun_AFailedPassDoesNotStopTheLoop covers the reason Reconcile's error is
// logged rather than returned: the loop outlives any single pass.
func TestRun_AFailedPassDoesNotStopTheLoop(t *testing.T) {
	store, _ := newBucket(t, "reconciler-failure")
	p := newPasses()
	var calls atomic.Int64

	run(t, reconciler.Config{
		Name:    "failure",
		Sources: []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) {
			p.record()
			if calls.Add(1) == 1 {
				return 0, assert.AnError
			}
			return 0, nil
		},
		Resync: 150 * time.Millisecond,
	})

	p.waitFor(t, 3, "passes after the first one failed")
}

// TestRun_AFilterExcludesOtherKeys stops an unrelated write to the same bucket
// waking a loop that does not care about it — the ELBv2 bucket holds listeners
// and rules alongside the load balancers DNS reconciles on.
func TestRun_AFilterExcludesOtherKeys(t *testing.T) {
	store, _ := newBucket(t, "reconciler-filter")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "filter",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, "lb.*")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    time.Hour,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "rule.one", &record{Name: "rule"}))
	p.stillAt(t, 1, "a write outside the filter must not trigger a pass")

	require.NoError(t, store.Set(t.Context(), "lb.one", &record{Name: "lb"}))
	p.waitFor(t, 2, "the pass triggered by a write inside the filter")
}

// bucketSet backs a Dynamic source in the per-account bucket case: the set is
// not known at startup and is re-read on every resync.
type bucketSet struct {
	mu      sync.Mutex
	buckets []*kvstore.Bucket
}

func (s *bucketSet) list(context.Context) ([]*kvstore.Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*kvstore.Bucket(nil), s.buckets...), nil
}

func (s *bucketSet) add(b *kvstore.Bucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = append(s.buckets, b)
}

// TestRun_ABucketAppearingAfterStartupIsPickedUp is the per-account bucket
// case: EKS and RDS create a bucket per account, and JetStream has no
// bucket-created event, so discovery rides on the resync.
func TestRun_ABucketAppearingAfterStartupIsPickedUp(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	first := kvstore.New[record](js, kvstore.Config{Name: "reconciler-acct-a", History: 1})
	second := kvstore.New[record](js, kvstore.Config{Name: "reconciler-acct-b", History: 1})

	set := &bucketSet{}
	set.add(first.Bucket)
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "dynamic",
		Sources:   []reconciler.Source{reconciler.Dynamic(set.list, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    200 * time.Millisecond,
	})
	p.waitFor(t, 1, "the startup pass")

	// Force the second bucket into existence, then publish it to the source.
	require.NoError(t, second.Set(t.Context(), "seed", &record{Name: "seed"}))
	set.add(second.Bucket)

	// One resync to discover it, then a write it must now notice.
	before := p.now()
	p.waitFor(t, before+2, "resync passes covering the discovery of the new bucket")

	before = p.now()
	require.NoError(t, second.Set(t.Context(), "later", &record{Name: "later"}))
	p.waitFor(t, before+1, "a pass triggered by the newly discovered bucket")
}

// TestRun_AnUnwatchableSourceStillResyncs covers the fallback: a bucket with no
// JetStream behind it cannot be watched, and the loop must degrade to the timer
// rather than stop.
func TestRun_AnUnwatchableSourceStillResyncs(t *testing.T) {
	unwatchable := kvstore.NewBucket(nil, kvstore.Config{
		Name:    "reconciler-unwatchable",
		Missing: "no JetStream configured",
	})
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "unwatchable",
		Sources:   []reconciler.Source{reconciler.Fixed(unwatchable, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Resync:    150 * time.Millisecond,
	})

	p.waitFor(t, 3, "resync passes despite the watch never establishing")
}

func TestFixed_ReportsItsBucketAndFilter(t *testing.T) {
	bucket := kvstore.NewBucket(nil, kvstore.Config{Name: "b"})
	src := reconciler.Fixed(bucket, "node.*")

	buckets, err := src.Buckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "b", buckets[0].Name())
	assert.Equal(t, "node.*", src.Filter())
}

func TestBucket_WatchOnANilJetStreamReportsTheConfiguredMessage(t *testing.T) {
	bucket := kvstore.NewBucket(nil, kvstore.Config{
		Name:    "b",
		Missing: "watch test: no JetStream client configured",
	})

	_, err := bucket.Watch(t.Context(), ">")
	require.EqualError(t, err, "watch test: no JetStream client configured")
}

// TestBucket_WatchDeliversUpdatesOnly pins UpdatesOnly at the kvstore seam: a
// watcher established over a bucket that already holds keys must not replay
// them, or every re-establish would fire a redundant pass per existing key.
func TestBucket_WatchDeliversUpdatesOnly(t *testing.T) {
	store, _ := newBucket(t, "reconciler-updatesonly")
	require.NoError(t, store.Set(t.Context(), "existing", &record{Name: "existing"}))

	watcher, err := store.Watch(t.Context(), ">")
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Stop() })

	require.NoError(t, store.Set(t.Context(), "fresh", &record{Name: "fresh"}))

	key := waitForUpdate(t, watcher)
	assert.Equal(t, "fresh", key, "a pre-existing key must not be replayed")
}

// waitForUpdate returns the key of the next real update, skipping the nil that
// marks the end of the initial replay.
func waitForUpdate(t *testing.T, watcher jetstream.KeyWatcher) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case update := <-watcher.Updates():
			if update == nil {
				continue
			}
			return update.Key()
		case <-deadline:
			t.Fatal("timed out waiting for a watch update")
			return ""
		}
	}
}
