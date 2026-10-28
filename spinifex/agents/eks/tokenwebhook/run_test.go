package tokenwebhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runConfig returns a Config whose files live under a fresh temp dir and whose
// static creds keep eksgw.New off the IMDS path.
func runConfig(t *testing.T, addr string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Addr:        addr,
		GatewayURL:  "https://127.0.0.1:1",
		AccessKey:   "AKIDTEST",
		SecretKey:   "secret",
		AccountID:   "000000000001",
		ClusterName: "toc",
		CertPath:    filepath.Join(dir, "webhook.crt"),
		KeyPath:     filepath.Join(dir, "webhook.key"),
		Kubeconfig:  filepath.Join(dir, "webhook.kubeconfig"),
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestRunReportsServingCertFailure(t *testing.T) {
	cfg := runConfig(t, freeAddr(t))
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	cfg.CertPath = filepath.Join(blocker, "webhook.crt")

	err := Run(context.Background(), cfg)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "serving cert: "), err.Error())
	assert.NoFileExists(t, cfg.Kubeconfig)
}

func TestRunReportsKubeconfigWriteFailure(t *testing.T) {
	cfg := runConfig(t, freeAddr(t))
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	cfg.Kubeconfig = filepath.Join(blocker, "webhook.kubeconfig")

	err := Run(context.Background(), cfg)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "write apiserver kubeconfig: "), err.Error())
}

// The kubeconfig is written before the gateway client is built, so k3s can
// still be pointed at the webhook when the gateway client fails.
func TestRunReportsGatewayClientFailureAfterWritingKubeconfig(t *testing.T) {
	cfg := runConfig(t, freeAddr(t))
	cfg.GatewayURL = ""

	err := Run(context.Background(), cfg)
	require.EqualError(t, err, "build gateway client: eksgw: baseURL is required")
	assert.FileExists(t, cfg.Kubeconfig)
}

func TestRunReportsListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()

	err = Run(context.Background(), runConfig(t, l.Addr().String()))
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "serve: "), err.Error())
}

func TestRunServesUntilCancelled(t *testing.T) {
	cfg := runConfig(t, freeAddr(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	var client *http.Client
	require.Eventually(t, func() bool {
		if client == nil {
			certPEM, err := os.ReadFile(cfg.CertPath)
			if err != nil {
				return false
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(certPEM) {
				return false
			}
			client = &http.Client{Timeout: time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			}}
		}
		resp, err := client.Get("https://" + cfg.Addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond, "webhook never served /healthz")

	kubeconfig, err := os.ReadFile(cfg.Kubeconfig)
	require.NoError(t, err)
	assert.Contains(t, string(kubeconfig), "https://"+cfg.Addr+"/authenticate")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunServesReadinessOnSeparateListener(t *testing.T) {
	g := newTLSGateway(t)
	cfg := runConfig(t, freeAddr(t))
	cfg.GatewayURL = "https://" + g.addr
	cfg.GatewayCA = g.caPath
	cfg.APIServerAddr = g.api.addr
	cfg.ReadyAddr = freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	client := &http.Client{Timeout: 3 * time.Second}
	require.Eventually(t, func() bool {
		resp, err := client.Get("http://" + cfg.ReadyAddr + "/readyz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "readiness listener never reported ready")

	resp, err := client.Post("http://"+cfg.ReadyAddr+"/authenticate", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the readiness listener must not serve /authenticate")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunReportsReadinessListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()

	cfg := runConfig(t, freeAddr(t))
	cfg.ReadyAddr = l.Addr().String()

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "serve readiness: "), err.Error())
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept serving after the readiness listener failed")
	}
}
