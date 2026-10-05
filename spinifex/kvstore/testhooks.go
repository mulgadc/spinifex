package kvstore

import (
	"testing"
	"time"
)

// ShortenOpenRetryForTest sets the OpenWithRetry pause to d for one test and
// restores it afterwards. There is one pause per process, so the test must not
// run in parallel with anything else that opens through OpenWithRetry.
func ShortenOpenRetryForTest(tb testing.TB, d time.Duration) {
	tb.Helper()
	tb.Cleanup(swapOpenRetryInterval(d))
}

// swapOpenRetryInterval sets the pause to d and returns a function restoring
// the previous value.
func swapOpenRetryInterval(d time.Duration) func() {
	prev := openRetryInterval
	openRetryInterval = d
	return func() { openRetryInterval = prev }
}
