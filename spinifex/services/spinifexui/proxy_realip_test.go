package spinifexui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proxyHeaders sends one request through the awsgw proxy from remoteAddr and
// returns the headers the backend received.
func proxyHeaders(t *testing.T, remoteAddr string, set map[string]string) http.Header {
	t.Helper()
	var got http.Header
	backend := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	t.Cleanup(backend.Close)

	transport, ok := backend.Client().Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport")
	proxy := newReverseProxy(backend.Listener.Addr().String(), "/proxy/awsgw", transport)

	req := httptest.NewRequest(http.MethodPost, "/proxy/awsgw/", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range set {
		req.Header.Set(k, v)
	}
	proxy.ServeHTTP(httptest.NewRecorder(), req)
	require.NotNil(t, got, "backend was not reached")
	return got
}

// Behind nginx the UI's own connection is loopback, so the edge's X-Real-IP and
// X-Request-ID reach awsgw unchanged.
func TestNewReverseProxy_PassesEdgeHeadersFromLoopback(t *testing.T) {
	got := proxyHeaders(t, "127.0.0.1:50000", map[string]string{
		"X-Real-IP":    "198.51.100.7",
		"X-Request-ID": "4f1c2b7e9a0d4e6f8b3c5a7d9e1f2a3b",
	})
	assert.Equal(t, "198.51.100.7", got.Get("X-Real-IP"))
	assert.Equal(t, "4f1c2b7e9a0d4e6f8b3c5a7d9e1f2a3b", got.Get("X-Request-ID"))
}

// The UI listens publicly. A client talking to it directly must not be able to
// hand awsgw a forged X-Real-IP over the loopback hop, where awsgw trusts it.
func TestNewReverseProxy_OverwritesSpoofedRealIPFromPublicClient(t *testing.T) {
	got := proxyHeaders(t, "203.0.113.9:50000", map[string]string{
		"X-Real-IP":       "10.0.0.1",
		"X-Forwarded-For": "127.0.0.1",
	})
	assert.Equal(t, "203.0.113.9", got.Get("X-Real-IP"))
	assert.Empty(t, got.Get("X-Forwarded-For"), "Rewrite drops inbound X-Forwarded-For")
}
