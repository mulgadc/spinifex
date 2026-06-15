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

// RetryWithReset runs fn as "attempt-1"; on failure it calls ResetAllNodes and
// retries as "attempt-2". The retry is a diagnostic aid, not a flake mask.
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
