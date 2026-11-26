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

// SetActiveNodesTTLForTest changes the background refresh interval for one
// test. Tests that call it must not run in parallel.
func SetActiveNodesTTLForTest(tb testing.TB, d time.Duration) {
	tb.Helper()
	prev := activeNodesTTL
	activeNodesTTL = d
	tb.Cleanup(func() { activeNodesTTL = prev })
}
