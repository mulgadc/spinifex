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

// AgeActiveNodesForTest makes the stored node count look older than the
// cache TTL, so a test can send a request after expiry without waiting.
func AgeActiveNodesForTest(d *NodeDiscovery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = d.at.Add(-activeNodesTTL - time.Second)
}
