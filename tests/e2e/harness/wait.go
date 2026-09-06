//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Eventually polls cond until it returns true or the timeout expires.
// Replaces every `sleep N; <probe>` loop in the bash scripts.
func Eventually(t *testing.T, cond func() bool, timeout, interval time.Duration, msgAndArgs ...any) {
	t.Helper()
	require.Eventually(t, cond, timeout, interval, msgAndArgs...)
}

// EventuallyErr is like Eventually but lets cond return an error for the
// failure message. Useful when the probe itself yields diagnostic detail.
//
// cond runs on the test's own goroutine, so a t.Fatal or a require inside it
// aborts the test as written. Under testify's EventuallyWithT it did not: the
// Goexit killed only the tick goroutine and left the collector empty, which
// reads as the condition being satisfied — so the wait returned successfully on
// a resource it had just declared broken, and the test carried on against it.
//
// The cost is that a cond which never returns hangs past the timeout instead of
// being abandoned. Every cond here is a bounded client call, and the go test
// timeout names the blocked call in its stack — better evidence than a bare
// "condition never satisfied" would be.
func EventuallyErr(t *testing.T, cond func() error, timeout, interval time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := cond()
		if err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("condition not met in %s: %v", timeout, err)
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
