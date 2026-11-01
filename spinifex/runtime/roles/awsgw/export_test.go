package awsgw

import (
	"testing"
	"time"
)

// SetDiscoverActiveNodesTimeoutForTest shortens the discovery fan-out for one
// test. Tests that call it must not run in parallel.
func SetDiscoverActiveNodesTimeoutForTest(tb testing.TB, d time.Duration) {
	tb.Helper()
	prev := discoverActiveNodesTimeout
	discoverActiveNodesTimeout = d
	tb.Cleanup(func() { discoverActiveNodesTimeout = prev })
}
