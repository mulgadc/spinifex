package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// stubReconciler returns a canned outcome per pass, so the requeue schedule can
// be driven without a clock or a live OVN.
type stubReconciler struct {
	outcomes []error
	calls    int
}

var _ Reconciler = (*stubReconciler)(nil)

func (s *stubReconciler) Reconcile(context.Context, IntentState) error {
	err := s.outcomes[min(s.calls, len(s.outcomes)-1)]
	s.calls++
	return err
}

func (s *stubReconciler) ReconcileApplyOnly(ctx context.Context, intent IntentState) error {
	return s.Reconcile(ctx, intent)
}

// An incomplete pass must requeue on a short backoff rather than wait a full
// DriftInterval: that gap is what left a VPC with no external gateway for five
// minutes after a 57-second DHCP stall.
func TestDrift_IncompletePassBacksOffThenResets(t *testing.T) {
	incomplete := fmt.Errorf("reconcile: 1 resource(s) unconverged: %w", ErrPassIncomplete)
	rec := &stubReconciler{outcomes: []error{
		incomplete, incomplete, incomplete, incomplete, incomplete, incomplete, nil,
	}}

	// Mirrors DriftLoop's body: each pass sizes the wait before the next.
	var backoff time.Duration
	var waits []time.Duration
	for range rec.outcomes {
		backoff = nextDriftBackoff(backoff, rec.Reconcile(t.Context(), IntentState{}))
		waits = append(waits, driftWait(backoff))
	}

	want := []time.Duration{
		5 * time.Second, 15 * time.Second, 45 * time.Second,
		135 * time.Second, DriftInterval, DriftInterval, // capped, then held
		DriftInterval, // converged: back to the routine interval
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("wait after pass %d = %v, want %v (full sequence %v)", i+1, waits[i], want[i], waits)
		}
	}
	// The cap and the reset produce the same wait, so pin the state behind it:
	// a converged pass must clear the backoff, not sit at the cap.
	if backoff != 0 {
		t.Errorf("backoff after a converged pass = %v, want 0", backoff)
	}
}

// A pass that converged, or failed for a reason other than non-convergence
// (e.g. a scan failure), must not shorten the interval — only an incomplete
// pass has a known-transient resource worth retrying early.
func TestDrift_NonIncompleteOutcomesKeepDriftInterval(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"converged", nil},
		{"scan failure", errors.New("scan actual OVN state: ovsdb unreachable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start from a backoff already in force, so a reset is observable.
			if got := nextDriftBackoff(45*time.Second, tt.err); got != 0 {
				t.Errorf("nextDriftBackoff(45s, %v) = %v, want 0", tt.err, got)
			}
			if got := driftWait(0); got != DriftInterval {
				t.Errorf("driftWait(0) = %v, want %v", got, DriftInterval)
			}
		})
	}
}

// The backoff must never exceed DriftInterval: a permanently broken resource
// degrades to today's behaviour instead of hammering the reconcile loop.
func TestDrift_BackoffCapsAtDriftInterval(t *testing.T) {
	incomplete := fmt.Errorf("unconverged: %w", ErrPassIncomplete)
	backoff := DriftInterval
	for range 3 {
		backoff = nextDriftBackoff(backoff, incomplete)
		if backoff > DriftInterval {
			t.Fatalf("backoff = %v, want <= DriftInterval (%v)", backoff, DriftInterval)
		}
	}
}
