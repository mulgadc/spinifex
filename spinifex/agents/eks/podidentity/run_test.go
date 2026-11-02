package podidentity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// startAPI serves a fake kube-apiserver whose certificate is signed by the k3s
// server CA, as the real one is.
func startAPI(t *testing.T, cfg CertConfig) *httptest.Server {
	t.Helper()
	other := cfg
	other.Dir = t.TempDir()
	certs, err := EnsureCerts(other)
	require.NoError(t, err)
	pair, err := tls.LoadX509KeyPair(certs.ServingCert, certs.ServingKey)
	require.NoError(t, err)
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "" {
			return
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	api.StartTLS()
	t.Cleanup(api.Close)
	return api
}

func TestRun_ServesTheWebhookStagesManifestsAndStopsOnCancel(t *testing.T) {
	cfg, tlsDir := certTestConfig(t)
	api := startAPI(t, cfg)
	dir := t.TempDir()
	roots := filepath.Join(dir, "roots.pem")
	require.NoError(t, os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0o644))
	c := ServiceConfig{
		Addr: freeAddr(t), Dir: cfg.Dir, TLSDir: tlsDir, APIServer: api.URL,
		Region: "ap-southeast-2", STSEndpoint: "https://gw:9999",
		SystemRoots: roots, ManifestDir: filepath.Join(dir, "manifests"), Resync: time.Hour,
	}
	require.NoError(t, os.MkdirAll(c.ManifestDir, 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, slog.New(slog.DiscardHandler)) }()

	// The API server calls the webhook over TLS, trusting only the server CA.
	caPEM, err := os.ReadFile(filepath.Join(tlsDir, "server-ca.crt"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	require.Eventually(t, func() bool {
		resp, err := client.Get("https://" + c.Addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond)

	_, err = os.Stat(filepath.Join(c.ManifestDir, manifestFile))
	assert.NoError(t, err, "the RBAC and webhook configuration are staged for k3s")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestRun_FailsWhenTheServerCAIsMissing(t *testing.T) {
	cfg, tlsDir := certTestConfig(t)
	require.NoError(t, os.Remove(filepath.Join(tlsDir, "server-ca.key")))
	c := ServiceConfig{
		Addr: freeAddr(t), Dir: cfg.Dir, TLSDir: tlsDir, APIServer: "https://127.0.0.1:1",
		Region: "r", STSEndpoint: "https://gw", SystemRoots: filepath.Join(t.TempDir(), "none"), ManifestDir: t.TempDir(),
	}
	err := Run(context.Background(), c, slog.New(slog.DiscardHandler))
	assert.Error(t, err, "the process exits so the supervisor retries until k3s has written its CAs")
}

func TestRun_RejectsANonLoopbackAddress(t *testing.T) {
	err := Run(context.Background(), ServiceConfig{Addr: "0.0.0.0:8445", Region: "r", STSEndpoint: "s"}, slog.New(slog.DiscardHandler))
	assert.Error(t, err)
}
