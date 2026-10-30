package gateway

import (
	"testing"
	"time"
)

// AdvertisedEndpoint exposes the gateway's own dialable base URL to the
// external test package.
func (gw *GatewayConfig) AdvertisedEndpoint() string { return gw.advertisedEndpoint() }

// SetDiscoverActiveNodesTimeoutForTest shortens the discovery fan-out for one
// test. Tests that call it must not run in parallel.
func SetDiscoverActiveNodesTimeoutForTest(tb testing.TB, d time.Duration) {
	tb.Helper()
	prev := discoverActiveNodesTimeout
	discoverActiveNodesTimeout = d
	tb.Cleanup(func() { discoverActiveNodesTimeout = prev })
}
