//test:in-package — the backoff ladder is unexported state (driftTiming,
//nextBackoff) and the tests drive runDriftCycle directly to pin what
//DriftLoop does with its return value.

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
)

// stubReconciler returns a canned outcome per pass, so the requeue schedule can
// be driven without a live OVN. Safe for concurrent use: DriftLoop calls it from
// its own goroutine while the test polls the count.
type stubReconciler struct {
	mu       sync.Mutex
	outcomes []error
	calls    int
}

var _ Reconciler = (*stubReconciler)(nil)

func (s *stubReconciler) Reconcile(context.Context, IntentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.outcomes[min(s.calls, len(s.outcomes)-1)]
	s.calls++
	return err
}

func (s *stubReconciler) ReconcileApplyOnly(ctx context.Context, intent IntentState) error {
	return s.Reconcile(ctx, intent)
}

// Deliberately not counted: the host EIP pass runs on its own loop, and folding
// it into the drift count would hide whether the leader gate still holds.
func (s *stubReconciler) ReconcileHostEIPs(context.Context) error { return nil }

func (s *stubReconciler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func incompleteErr() error {
	return fmt.Errorf("reconcile: 1 resource(s) unconverged: %w", ErrPassIncomplete)
}

// shrunkDriftTiming compresses the loop's timings so a test observes several
// requeues in milliseconds. The interval stays long so that a loop which
// wrongly falls back to it registers as silence rather than as a fast requeue.
func shrunkDriftTiming(interval, base time.Duration) driftTiming {
	tm := defaultDriftTiming()
	tm.interval, tm.base = interval, base
	return tm
}

// startDriftLoop runs the loop on tm and joins it on cleanup. A zero tm runs
// the exported DriftLoop on the package defaults.
func startDriftLoop(t *testing.T, rec Reconciler, nc *nats.Conn, startup error, tm driftTiming) {
	t.Helper()
	startDriftLoopTriggered(t, rec, nc, startup, nil, tm)
}

func startDriftLoopTriggered(t *testing.T, rec Reconciler, nc *nats.Conn, startup error, trigger <-chan struct{}, tm driftTiming) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		lease := vpcdLease(testutil.NewJetStream(t, nc), "node-1")
		if tm == (driftTiming{}) {
			DriftLoop(ctx, rec, nc, lease, "us-east-1a", startup, trigger)
			return
		}
		driftLoop(ctx, rec, nc, lease, "us-east-1a", startup, trigger, tm)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("DriftLoop still running 5s after cancel, want a return on ctx.Done()")
		}
	})
}

// waitForCalls polls until rec has been called want times or within elapses.
func waitForCalls(t *testing.T, rec *stubReconciler, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := rec.callCount(); got >= want {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return rec.callCount()
}

// The whole point of the change: an incomplete pass must come back in
// milliseconds-scaled backoff rather than a full DriftInterval. Driving the real
// DriftLoop is what pins timer.Reset to the computed wait — a loop that resets
// to DriftInterval instead still satisfies every pure-function test below.
func TestDriftLoop_IncompletePassRequeuesFast(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	tm := shrunkDriftTiming(10*time.Second, time.Millisecond)

	rec := &stubReconciler{outcomes: []error{incompleteErr()}}
	startDriftLoop(t, rec, nc, incompleteErr(), tm)

	if got := waitForCalls(t, rec, 4, 5*time.Second); got < 4 {
		t.Fatalf("reconcile called %d times, want >= 4 — an incomplete pass is waiting "+
			"a full DriftInterval instead of requeueing on the backoff", got)
	}
}

// The startup ReconcileApplyOnly pass is where the gateway LRP's first DORA
// happens. Its outcome must seed the loop, or the resource it failed to converge
// waits a full DriftInterval for its first retry — the original bug, on boot.
func TestDriftLoop_SeedsBackoffFromStartupOutcome(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	tm := shrunkDriftTiming(10*time.Second, time.Millisecond)

	// Converged from here on: only the seed can bring the first pass forward.
	rec := &stubReconciler{outcomes: []error{nil}}
	startDriftLoop(t, rec, nc, incompleteErr(), tm)

	if got := waitForCalls(t, rec, 1, 5*time.Second); got < 1 {
		t.Fatalf("reconcile never ran: a failed startup pass must seed the backoff "+
			"rather than leave the loop armed at DriftInterval (calls=%d)", got)
	}
}

// The converse: a clean startup must not pull the first pass forward, or every
// vpcd reconciles immediately on boot regardless of need.
func TestDriftLoop_ConvergedStartupWaitsFullInterval(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	tm := shrunkDriftTiming(10*time.Second, time.Millisecond)

	rec := &stubReconciler{outcomes: []error{incompleteErr()}}
	startDriftLoop(t, rec, nc, nil, tm)

	time.Sleep(100 * time.Millisecond)
	if got := rec.callCount(); got != 0 {
		t.Errorf("reconcile ran %d times within 100ms of a converged startup, want 0 "+
			"— the first pass must wait DriftInterval (10s here)", got)
	}
}

// A converged pass must clear the backoff so the loop drops back to its routine
// cadence, rather than staying on the fast requeue forever.
func TestDriftLoop_ConvergedPassReturnsToDriftInterval(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	tm := shrunkDriftTiming(10*time.Second, time.Millisecond)

	// Pass 1 incomplete (requeues in ~3ms), pass 2 converged (must requeue at 10s).
	rec := &stubReconciler{outcomes: []error{incompleteErr(), nil}}
	startDriftLoop(t, rec, nc, incompleteErr(), tm)

	if got := waitForCalls(t, rec, 2, 5*time.Second); got < 2 {
		t.Fatalf("reconcile called %d times, want 2 before the loop settles", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.callCount(); got != 2 {
		t.Errorf("reconcile called %d times, want exactly 2 — a converged pass must "+
			"reset the backoff to DriftInterval, not stay on the fast requeue", got)
	}
}

// runDriftCycle's return is what sizes the next wait, so a pass this node did not
// win must not be reported as converged-or-broken; it reports nil and the caller
// leaves the cadence alone.
func TestRunDriftCycle_ReportsReconcileOutcome(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)

	rec := &stubReconciler{outcomes: []error{incompleteErr()}}
	_, err := runDriftCycle(t.Context(), rec, vpcdLease(js, "node-1"), js, "us-east-1a")
	if !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("runDriftCycle = %v, want ErrPassIncomplete propagated from the pass", err)
	}

	converged := &stubReconciler{outcomes: []error{nil}}
	if _, err := runDriftCycle(t.Context(), converged, vpcdLease(js, "node-1"), js, "us-east-1a"); err != nil {
		t.Errorf("runDriftCycle = %v, want nil for a converged pass", err)
	}
}

// A cycle this node loses the election for must not run a pass at all.
func TestRunDriftCycle_SkipsWhenNotElected(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)

	release, ok := AcquireLeader(t.Context(), vpcdLease(js, "other-node"))
	if !ok {
		t.Fatal("failed to take the lock the test needs held")
	}
	defer release()

	rec := &stubReconciler{outcomes: []error{incompleteErr()}}
	retry, err := runDriftCycle(t.Context(), rec, vpcdLease(js, "node-1"), js, "us-east-1a")
	if err != nil {
		t.Errorf("runDriftCycle = %v, want nil when not elected", err)
	}
	if retry <= 0 || retry > LeaderTTL/3 {
		t.Errorf("revisit = %v, want within %v so a dead holder is taken over promptly", retry, LeaderTTL/3)
	}
	if got := rec.callCount(); got != 0 {
		t.Errorf("reconcile ran %d times without winning the election, want 0", got)
	}
}

// The ladder itself, as a pure function: exact gaps are asserted here because
// the loop tests above run on compressed timings and can only prove ordering.
// Zero is the converged value and means no deadline, which leaves the next pass
// to the resync the loop configures at DriftInterval.
func TestDrift_IncompletePassBacksOffThenResets(t *testing.T) {
	t.Parallel()
	incomplete := incompleteErr()
	outcomes := []error{incomplete, incomplete, incomplete, incomplete, incomplete, incomplete, nil}

	var backoff time.Duration
	var got []time.Duration
	for _, outcome := range outcomes {
		backoff = defaultDriftTiming().nextBackoff(backoff, outcome)
		got = append(got, backoff)
	}

	want := []time.Duration{
		5 * time.Second, 15 * time.Second, 45 * time.Second,
		135 * time.Second, DriftInterval, DriftInterval, // capped, then held
		0, // converged: no deadline, the resync takes over
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff after pass %d = %v, want %v (full sequence %v)", i+1, got[i], want[i], got)
		}
	}
}

// A pass that converged, or failed for a reason other than non-convergence
// (e.g. a scan failure), must not shorten the interval — only an incomplete
// pass has a known-transient resource worth retrying early.
func TestDrift_NonIncompleteOutcomesKeepDriftInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{"converged", nil},
		{"scan failure", errors.New("scan actual OVN state: ovsdb unreachable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Start from a backoff already in force, so a reset is observable.
			if got := defaultDriftTiming().nextBackoff(45*time.Second, tt.err); got != 0 {
				t.Errorf("nextBackoff(45s, %v) = %v, want 0", tt.err, got)
			}
		})
	}
}

// The backoff must never exceed DriftInterval: a permanently broken resource
// degrades to today's behaviour instead of hammering the reconcile loop.
func TestDrift_BackoffCapsAtDriftInterval(t *testing.T) {
	t.Parallel()
	incomplete := incompleteErr()
	backoff := DriftInterval
	for range 3 {
		backoff = defaultDriftTiming().nextBackoff(backoff, incomplete)
		if backoff > DriftInterval {
			t.Fatalf("backoff = %v, want <= DriftInterval (%v)", backoff, DriftInterval)
		}
	}
}
