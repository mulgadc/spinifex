package reconciler_test

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reason the trigger exists: a loop whose signal arrives by some other route
// than a KV write would otherwise see it only when the next pass happened to
// look, which is the poll the watch was supposed to replace.
func TestRun_ATriggerWakesTheLoopWithNoWatchTraffic(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-wakes")
	p := newPasses()
	trigger := make(chan struct{}, 1)

	run(t, reconciler.Config{
		Name:      "trigger",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Trigger:   trigger,
		Resync:    time.Hour,
	})

	p.waitFor(t, 1, "the startup pass")
	trigger <- struct{}{}

	p.waitFor(t, 2, "a pass from the trigger, against a bucket nobody is writing to")
}

// A quiet trigger must not be a source of passes on its own: the loop still
// falls back to the resync and nothing else.
func TestRun_AQuietTriggerAddsNoPasses(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-quiet")
	p := newPasses()
	trigger := make(chan struct{}, 1)

	run(t, reconciler.Config{
		Name:      "quiet",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Trigger:   trigger,
		Resync:    time.Hour,
	})

	p.waitFor(t, 1, "the startup pass")
	p.stillAt(t, 1, "a trigger nobody signalled must not produce passes")
}

// A burst of triggers is one pass, the same as a burst of watch updates. The
// caller signalling several times in a row is describing one thing to converge,
// not several.
func TestRun_ABurstOfTriggersCoalescesIntoOnePass(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-burst")
	p := newPasses()
	trigger := make(chan struct{}, 8)

	run(t, reconciler.Config{
		Name:      "burst",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Trigger:   trigger,
		Resync:    time.Hour,
		Debounce:  100 * time.Millisecond,
	})

	p.waitFor(t, 1, "the startup pass")
	for range 8 {
		trigger <- struct{}{}
	}

	p.waitFor(t, 2, "one pass for the burst")
	p.stillAt(t, 2, "eight triggers inside one debounce window are one pass")
}

// A trigger names no key, so a per-key loop has to be served by the whole-set
// pass. Anything else would leave the caller's signal describing nothing.
func TestRun_ATriggerRunsTheWholeSetOnAPerKeyLoop(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-whole-set")
	whole := newPasses()
	keyed := newPasses()
	trigger := make(chan struct{}, 1)

	run(t, reconciler.Config{
		Name:         "whole-set",
		Sources:      []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile:    func(context.Context) (time.Duration, error) { whole.record(); return 0, nil },
		ReconcileKey: func(context.Context, string) (time.Duration, error) { keyed.record(); return 0, nil },
		Trigger:      trigger,
		Resync:       time.Hour,
	})

	whole.waitFor(t, 1, "the startup pass")
	trigger <- struct{}{}

	whole.waitFor(t, 2, "the trigger served by a whole-set pass")
	assert.Zero(t, keyed.now(), "a trigger carries no key, so no per-key pass may run")
}

// The deadline a triggered pass returns has to be armed like any other, or a
// loop woken by a trigger would silently lose the schedule it just asked for.
func TestRun_ATriggeredPassStillArmsItsDeadline(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-deadline")
	p := newPasses()
	trigger := make(chan struct{}, 1)

	run(t, reconciler.Config{
		Name:      "trigger-deadline",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 80 * time.Millisecond, nil },
		Resync:    time.Hour,
		Trigger:   trigger,
	})

	p.waitFor(t, 1, "the startup pass")
	trigger <- struct{}{}

	p.waitFor(t, 5, "deadline passes continuing after the trigger woke the loop")
}

// Closing the trigger is how a caller says its source is finished. The loop must
// survive it rather than spin on a closed channel.
func TestRun_AClosedTriggerDoesNotSpin(t *testing.T) {
	t.Parallel()
	store, _ := newBucket(t, "trigger-closed")
	p := newPasses()
	trigger := make(chan struct{})

	run(t, reconciler.Config{
		Name:      "closed",
		Sources:   []reconciler.Source{reconciler.Fixed(store.Bucket, ">")},
		Reconcile: func(context.Context) (time.Duration, error) { p.record(); return 0, nil },
		Trigger:   trigger,
		Resync:    time.Hour,
	})

	p.waitFor(t, 1, "the startup pass")
	close(trigger)

	// One pass is allowed for the close itself; what must not happen is the
	// forwarder reading a closed channel forever and passing on every read.
	time.Sleep(300 * time.Millisecond)
	require.LessOrEqual(t, p.now(), 2, "a closed trigger must not drive a pass loop")

	// Still alive: a write after the close is still reconciled.
	before := p.now()
	require.NoError(t, store.Set(t.Context(), "after-close", &record{Name: "after-close"}))
	p.waitFor(t, before+1, "a watch update after the trigger closed")
}
