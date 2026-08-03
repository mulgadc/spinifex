package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckPredastoreReady_HTTPSServing asserts that any HTTP response from
// predastore's endpoint counts as ready, including 403 — the response an S3
// endpoint gives this deliberately unsigned probe request. The prior TCP-dial
// probe treated a bare listening socket as ready and raced predastore's TLS
// handshake; this proves the replacement actually waits for HTTP.
func TestCheckPredastoreReady_HTTPSServing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	d := &Daemon{config: &config.Config{Predastore: config.PredastoreConfig{Host: u.Host}}}
	assert.True(t, d.checkPredastoreReady())
}

// TestCheckPredastoreReady_ClosedPort asserts a port nothing is listening on
// is reported not-ready, rather than panicking or failing open.
func TestCheckPredastoreReady_ClosedPort(t *testing.T) {
	// Bind then immediately close to get a real, currently-unused port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	d := &Daemon{config: &config.Config{Predastore: config.PredastoreConfig{Host: addr}}}
	assert.False(t, d.checkPredastoreReady())
}

// TestCheckPredastoreReady_NoPredastoreConfigured asserts the short-circuit:
// an empty Predastore.Host means no predastore runs on this node, and the
// check must return ready without dialling anything.
func TestCheckPredastoreReady_NoPredastoreConfigured(t *testing.T) {
	d := &Daemon{config: &config.Config{}}
	assert.True(t, d.checkPredastoreReady())
}
