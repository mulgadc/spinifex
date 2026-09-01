package reconciler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_ARevisitDeadlineFiresWithNoWatchTraffic is the reason the deadline
// exists: a loop waiting on something outside KV gets no watch update at all,
// so without this it would run only on the resync.
func TestRun_ARevisitDeadlineFiresWithNoWatchTraffic(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-deadline")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "deadline",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 80 * time.Millisecond, nil },
		Resync:    time.Hour,
	})

	p.waitFor(t, 4, "repeated deadline passes against a bucket nobody is writing to")
}

// TestRun_ARevisitDeadlineIsClampedToTheResync holds the one direction a caller
// may not move: the resync is the outer bound on staleness, so a longer deadline
// is ignored rather than allowed to defer past it.
func TestRun_ARevisitDeadlineIsClampedToTheResync(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-clamped")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "clamped",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return time.Hour, nil },
		Resync:    150 * time.Millisecond,
	})

	p.waitFor(t, 4, "resync passes despite a pass asking for an hour")
}

// TestRun_DroppingTheDeadlineStopsTheEarlyPasses covers a loop whose work
// finishes: once the last transitional resource settles it stops asking, and the
// loop must fall back to the resync rather than keep the old deadline armed.
func TestRun_DroppingTheDeadlineStopsTheEarlyPasses(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-dropped")
	p := newPasses()
	var calls atomic.Int64

	run(t, reconciler.Config{
		Name:    "dropped",
		Sources: []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) {
			p.record()
			if calls.Add(1) == 1 {
				return 50 * time.Millisecond, nil
			}
			return 0, nil
		},
		Resync: time.Hour,
	})

	p.waitFor(t, 2, "the pass the first deadline asked for")
	p.stillAt(t, 2, "a pass that asks for no deadline must not be revisited")
}

// TestRun_AFailedPassKeepsItsRevisitDeadline stops a failure demoting a loop to
// the resync. A pass that fails partway through a transition is exactly the one
// that most needs revisiting on its own schedule.
func TestRun_AFailedPassKeepsItsRevisitDeadline(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-failed")
	p := newPasses()

	run(t, reconciler.Config{
		Name:    "failed",
		Sources: []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) {
			p.record()
			return 80 * time.Millisecond, assert.AnError
		},
		Resync: time.Hour,
	})

	p.waitFor(t, 4, "deadline passes after each one failed")
}

// TestRun_AChangeBeforeTheDeadlineStillTriggersAPass keeps the deadline additive.
// The wait is capped well below the deadline asked for, so a pass arriving at all
// proves the change woke the loop rather than the timer.
func TestRun_AChangeBeforeTheDeadlineStillTriggersAPass(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-change-wins")
	p := newPasses()

	run(t, reconciler.Config{
		Name:      "change-wins",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return time.Minute, nil },
		Resync:    time.Hour,
	})
	p.waitFor(t, 1, "the startup pass")

	require.NoError(t, store.Set(t.Context(), "one", &record{Name: "one"}))
	p.waitFor(t, 2, "the pass triggered by the write, long before the deadline")
}

// TestRun_ADeadlinePassRunsTheWholeSet pins which half of a per-key loop a
// deadline serves. Nothing changed, so there is no key to name, and only a
// whole-set pass can work out what the next deadline should be.
func TestRun_ADeadlinePassRunsTheWholeSet(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "revisit-whole-set")
	p, keys := newPasses(), newKeyLog()

	run(t, reconciler.Config{
		Name:         "whole-set",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { p.record(); return 80 * time.Millisecond, nil },
		ReconcileKey: func(_ context.Context, key string) (time.Duration, error) { keys.record(key); return 0, nil },
		Resync:       time.Hour,
	})

	p.waitFor(t, 4, "deadline passes on the whole-set path")
	keys.stillAt(t, 0, "a deadline with nothing changed must not reconcile a key")
}
