//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"
)

// Eventually polls cond until it returns true or the timeout expires.
// Replaces every `sleep N; <probe>` loop in the bash scripts.
func Eventually(t *testing.T, cond func() bool, timeout, interval time.Duration, msgAndArgs ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			if len(msgAndArgs) == 0 {
				t.Fatalf("Eventually: condition not met within %s", timeout)
			}
			t.Fatalf("Eventually: condition not met within %s: %v", timeout, msgAndArgs)
		}
		time.Sleep(interval)
	}
}

// EventuallyErr is like Eventually but lets cond return an error for the
// failure message. Useful when the probe itself yields diagnostic detail.
func EventuallyErr(t *testing.T, cond func() error, timeout, interval time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := cond(); err == nil {
			return
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("EventuallyErr: condition not met within %s: %v", timeout, lastErr)
		}
		time.Sleep(interval)
	}
}

// RetryWithReset runs fn as an "attempt-1" subtest; on failure it calls
// ResetAllNodes and retries fn as "attempt-2". Both attempts surface in the
// test output so CI logs preserve the transient-vs-deterministic signal —
// the retry is a diagnostic aid, not a way to mask flakes.
//
// Used by DDIL's quarantine wrapper (ddil/harness.Run) and any other scenario
// that wants the same retry-after-reset shape without taking on DDIL's
// quarantine semantics.
func RetryWithReset(t *testing.T, c *Cluster, ssh SSH, label string, fn func(*testing.T)) {
	t.Helper()
	if t.Run("attempt-1", fn) {
		return
	}
	t.Logf("e2e harness: %s failed first attempt, resetting cluster and retrying", label)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ResetAllNodes(ctx, c, ssh); err != nil {
		t.Errorf("e2e harness: reset before retry of %s: %v", label, err)
		return
	}
	t.Run("attempt-2", fn)
}
