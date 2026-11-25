//test:in-package — drives the unexported branding proxy directly.
package spinifexui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brandingBackend starts a TLS backend and a transport that dials it for any
// host, as the proxy's fixed localhost:9999 would be.
func brandingBackend(t *testing.T, h http.HandlerFunc) http.RoundTripper {
	t.Helper()
	backend := httptest.NewTLSServer(h)
	t.Cleanup(backend.Close)
	transport := backend.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	}
	transport.TLSClientConfig.ServerName = "example.com"
	return transport
}

func TestBrandingProxy_ForwardsToTheFixedGatewayPath(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	transport := brandingBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(":root.dark{}"))
	})
	h := newBrandingProxy("localhost:9999", transport, time.Second)

	req := httptest.NewRequest(http.MethodGet, "/branding/branding.css?rev=3", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, ":root.dark{}", w.Body.String())
	assert.Equal(t, "/console/branding/branding.css", gotPath)
	assert.Equal(t, "rev=3", gotQuery)
	assert.Empty(t, gotAuth, "the public route needs no credential, so none is forwarded")
}

func TestBrandingProxy_ForwardsOnlyTheFixedNames(t *testing.T) {
	var hits atomic.Int32
	transport := brandingBackend(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	h := newBrandingProxy("localhost:9999", transport, time.Second)

	for _, path := range []string{"/branding/", "/branding/other", "/branding/logo/x", "/branding/..%2fadmin%2fListAccounts"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/branding/branding.json", strings.NewReader("{}")))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	assert.Zero(t, hits.Load(), "nothing outside the fixed names reaches the gateway")
}

func TestBrandingProxy_StalledGatewayReturnsWithinTheDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	transport := brandingBackend(t, func(http.ResponseWriter, *http.Request) { <-release })
	// Registered after the backend, so it runs first and lets Close finish.
	t.Cleanup(func() { close(release) })
	h := newBrandingProxy("localhost:9999", transport, 100*time.Millisecond)

	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/branding/branding.json", nil))

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Less(t, time.Since(start), time.Second)
}

func TestHandler_MountsTheBrandingRoute(t *testing.T) {
	transport := brandingBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	})
	h := (&Service{Config: &Config{Region: "us-west-1"}}).newHandler(nil, t.TempDir()+"/ca.pem", transport)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/branding/branding.json", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/console/branding/branding.json", w.Body.String())
}
