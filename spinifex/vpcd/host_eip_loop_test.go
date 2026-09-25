//test:in-package — runHostEIPLoop and its two timing vars are unexported,
// and the test has to shrink the interval to run at all.

package vpcd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
)

type countingReconciler struct {
	mu   sync.Mutex
	n    int
	err  error
	seen chan struct{}
}

var _ reconcile.Reconciler = (*countingReconciler)(nil)

func (c *countingReconciler) Reconcile(context.Context, reconcile.IntentState) error { return nil }

func (c *countingReconciler) ReconcileApplyOnly(context.Context, reconcile.IntentState) error {
	return nil
}

func (c *countingReconciler) ReconcileHostEIPs(context.Context) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	select {
	case c.seen <- struct{}{}:
	default:
	}
	return c.err
}

func (c *countingReconciler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func withShortHostEIPTiming(t *testing.T) {
	t.Helper()
	interval, delay := hostEIPInterval, hostEIPStartDelay
	hostEIPInterval, hostEIPStartDelay = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { hostEIPInterval, hostEIPStartDelay = interval, delay })
}

// The loop is what makes the host plumbing node-local: it runs on every vpcd,
// leader or not, so a node that never wins the reconcile lease still has a
// route to the guests it is running.
func TestRunHostEIPLoopKeepsPassing(t *testing.T) {
	withShortHostEIPTiming(t)
	rec := &countingReconciler{seen: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runHostEIPLoop(ctx, rec); close(done) }()

	for range 2 {
		select {
		case <-rec.seen:
		case <-time.After(2 * time.Second):
			t.Fatal("host EIP pass did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on context cancel")
	}
}

// A failed pass is a node with no inbound path, and the next tick is the only
// thing that repairs it — so the loop must log and carry on, never exit.
func TestRunHostEIPLoopSurvivesAFailedPass(t *testing.T) {
	withShortHostEIPTiming(t)
	rec := &countingReconciler{err: errors.New("NB unreachable"), seen: make(chan struct{}, 1)}
	go runHostEIPLoop(t.Context(), rec)

	deadline := time.After(2 * time.Second)
	for rec.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("loop stopped after %d failed passes", rec.count())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
