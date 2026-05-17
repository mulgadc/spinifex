//go:build e2e

package harness

import (
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
