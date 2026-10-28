package tokenwebhook

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tlsGateway is a loopback TLS listener standing in for the gateway. It counts
// accepted connections and can be stopped and restarted on the same address.
type tlsGateway struct {
	t       *testing.T
	api     *fakeAPIServer
	addr    string
	cert    tls.Certificate
	caPath  string
	ln      net.Listener
	accepts atomic.Int32
}

func newTLSGateway(t *testing.T) *tlsGateway {
	t.Helper()
	dir := t.TempDir()
	cert, certPEM, err := ensureServingCert(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"))
	require.NoError(t, err)
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, certPEM, 0o600))

	g := &tlsGateway{t: t, cert: cert, caPath: caPath, api: newFakeAPIServer(t)}
	g.start("127.0.0.1:0")
	t.Cleanup(g.stop)
	return g
}

func (g *tlsGateway) start(addr string) {
	g.t.Helper()
	ln, err := tls.Listen("tcp", addr, serverTLSConfig(g.cert))
	require.NoError(g.t, err)
	g.ln = ln
	g.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.accepts.Add(1)
			go func() {
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
				_ = c.Close()
			}()
		}
	}()
}

func (g *tlsGateway) stop() {
	if g.ln != nil {
		_ = g.ln.Close()
		g.ln = nil
	}
}

// fakeAPIServer is a plain TCP listener standing in for the local apiserver.
type fakeAPIServer struct {
	addr string
	ln   net.Listener
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	a := &fakeAPIServer{}
	a.start("127.0.0.1:0", t)
	t.Cleanup(a.stop)
	return a
}

func (a *fakeAPIServer) start(addr string, t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	a.ln = ln
	a.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

func (a *fakeAPIServer) stop() {
	if a.ln != nil {
		_ = a.ln.Close()
		a.ln = nil
	}
}

func (g *tlsGateway) probe(t *testing.T) *gatewayProbe {
	t.Helper()
	p, err := newGatewayProbe("https://"+g.addr, g.caPath, g.api.addr)
	require.NoError(t, err)
	return p
}

func TestGatewayProbe_UpAndRefused(t *testing.T) {
	g := newTLSGateway(t)
	p := g.probe(t)
	p.ttl = 0

	assert.True(t, p.ready())

	g.stop()
	assert.False(t, p.ready(), "a closed gateway port is not ready")
}

func TestGatewayProbe_RecoversAfterDown(t *testing.T) {
	g := newTLSGateway(t)
	p := g.probe(t)
	p.ttl = 0
	addr := g.addr

	g.stop()
	require.False(t, p.ready())

	g.start(addr)
	assert.True(t, p.ready(), "ready again once the gateway is back")
}

func TestGatewayProbe_TimesOutOnStalledHandshake(t *testing.T) {
	g := newTLSGateway(t)
	// A plain listener that accepts and never answers the TLS handshake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	p, err := newGatewayProbe("https://"+ln.Addr().String(), g.caPath, g.api.addr)
	require.NoError(t, err)
	p.timeout = 200 * time.Millisecond
	p.ttl = 0

	start := time.Now()
	assert.False(t, p.ready())
	assert.Less(t, time.Since(start), 2*time.Second, "the probe gives up at its timeout")
}

func TestGatewayProbe_RejectsUntrustedCertificate(t *testing.T) {
	g := newTLSGateway(t)
	other := newTLSGateway(t)
	p, err := newGatewayProbe("https://"+g.addr, other.caPath, g.api.addr)
	require.NoError(t, err)
	p.ttl = 0

	assert.False(t, p.ready(), "a certificate not signed by the configured CA is not ready")
}

func TestGatewayProbe_CachesWithinTTL(t *testing.T) {
	g := newTLSGateway(t)
	p := g.probe(t)
	clock := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return clock }
	p.ttl = 2 * time.Second

	require.True(t, p.ready())
	require.True(t, p.ready())
	require.True(t, p.ready())
	assert.Equal(t, int32(1), g.accepts.Load(), "calls inside the ttl share one probe")

	g.stop()
	assert.True(t, p.ready(), "a cached answer is served while the ttl holds")

	clock = clock.Add(3 * time.Second)
	assert.False(t, p.ready(), "after the ttl the gateway is probed again")
}

func TestGatewayProbe_HandlerStatus(t *testing.T) {
	g := newTLSGateway(t)
	p := g.probe(t)
	p.ttl = 0

	w := httptest.NewRecorder()
	p.handle(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, w.Code)

	g.stop()
	w = httptest.NewRecorder()
	p.handle(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestNewGatewayProbe_DefaultsPortAndRejectsBadInput(t *testing.T) {
	p, err := newGatewayProbe("https://gw.example", "", "")
	require.NoError(t, err)
	assert.Equal(t, "gw.example:443", p.addr)
	assert.Equal(t, defaultAPIServerAddr, p.apiAddr)

	_, err = newGatewayProbe("https://", "", "")
	require.Error(t, err)
	_, err = newGatewayProbe("https://gw.example:9999", "/no/such/ca.pem", "")
	require.Error(t, err)
}

func TestGatewayProbe_NotReadyWhenAPIServerDown(t *testing.T) {
	g := newTLSGateway(t)
	p := g.probe(t)
	p.ttl = 0
	require.True(t, p.ready())

	g.api.stop()
	assert.False(t, p.ready(), "a dead apiserver must not look ready while the gateway is up")

	w := httptest.NewRecorder()
	p.handle(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestReadinessHandler_ServesOnlyReadyz(t *testing.T) {
	g := newTLSGateway(t)
	h := readinessHandler(g.probe(t))

	for _, path := range []string{"/authenticate", "/healthz", "/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		assert.Equalf(t, http.StatusNotFound, w.Code, "%s must not be served on the readiness listener", path)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}
