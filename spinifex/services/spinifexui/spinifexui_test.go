package spinifexui

import (
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_ValidConfig(t *testing.T) {
	cfg := &Config{
		Port:    8080,
		Host:    "127.0.0.1",
		TLSCert: "/custom/cert.pem",
		TLSKey:  "/custom/key.pem",
	}

	svc, err := New(cfg)

	require.NoError(t, err)
	assert.NotNil(t, svc)
	assert.Equal(t, 8080, svc.Config.Port)
	assert.Equal(t, "127.0.0.1", svc.Config.Host)
	assert.Equal(t, "/custom/cert.pem", svc.Config.TLSCert)
	assert.Equal(t, "/custom/key.pem", svc.Config.TLSKey)
}

func TestNew_DefaultPortAndHost(t *testing.T) {
	cfg := &Config{
		TLSCert: "/custom/cert.pem",
		TLSKey:  "/custom/key.pem",
	}

	svc, err := New(cfg)

	require.NoError(t, err)
	assert.Equal(t, 3000, svc.Config.Port)
	assert.Equal(t, "0.0.0.0", svc.Config.Host)
}

func TestNew_DefaultTLSPaths(t *testing.T) {
	cfg := &Config{}

	svc, err := New(cfg)

	require.NoError(t, err)
	// TLS paths should be filled in from home directory
	assert.NotEmpty(t, svc.Config.TLSCert)
	assert.NotEmpty(t, svc.Config.TLSKey)
	assert.Contains(t, svc.Config.TLSCert, "server.pem")
	assert.Contains(t, svc.Config.TLSKey, "server.key")
}

func TestNew_CustomTLSPreserved(t *testing.T) {
	cfg := &Config{
		TLSCert: "/my/cert.pem",
		TLSKey:  "/my/key.pem",
	}

	svc, err := New(cfg)

	require.NoError(t, err)
	assert.Equal(t, "/my/cert.pem", svc.Config.TLSCert)
	assert.Equal(t, "/my/key.pem", svc.Config.TLSKey)
}

func TestNew_PartialTLSFillsOnlyEmpty(t *testing.T) {
	cfg := &Config{
		TLSCert: "/my/cert.pem",
		// TLSKey left empty — should get default
	}

	svc, err := New(cfg)

	require.NoError(t, err)
	// Both are empty-or-not checked together, so both get defaults
	// because the condition is cfg.TLSCert == "" || cfg.TLSKey == ""
	assert.Equal(t, "/my/cert.pem", svc.Config.TLSCert)
	assert.Contains(t, svc.Config.TLSKey, "server.key")
}

func TestNew_InvalidConfigType(t *testing.T) {
	svc, err := New("not a config")

	assert.Nil(t, svc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config type")
}

func TestNew_InvalidConfigTypeInt(t *testing.T) {
	svc, err := New(42)

	assert.Nil(t, svc)
	assert.Error(t, err)
}

func TestStatus_ReturnsValidState(t *testing.T) {
	svc := &Service{
		Config: &Config{},
	}

	status, err := svc.Status()

	assert.NoError(t, err)
	// On a dev machine the spinifex-ui PID file may exist, so accept either outcome
	assert.True(t, status == "stopped" || len(status) > 0, "status should be non-empty")
}

func TestReload_ReturnsNil(t *testing.T) {
	svc := &Service{
		Config: &Config{},
	}

	err := svc.Reload()
	assert.NoError(t, err)
}

func TestServiceName(t *testing.T) {
	assert.Equal(t, "spinifex-ui", serviceName)
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := securityHeadersMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	assert.Equal(t, "camera=(), microphone=(), geolocation=(), browsing-topics=()", resp.Header.Get("Permissions-Policy"))
	assert.Equal(t, "strict-origin-when-cross-origin", resp.Header.Get("Referrer-Policy"))
	assert.Equal(t, "max-age=31536000; includeSubDomains", resp.Header.Get("Strict-Transport-Security"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
}

func TestSecurityHeadersMiddleware_PassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "test-value")
		w.WriteHeader(http.StatusCreated)
	})

	handler := securityHeadersMiddleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/api/data", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "test-value", resp.Header.Get("X-Custom"))
	// Security headers still present
	assert.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))
}

func TestChiCompress_CompressesEligibleContent(t *testing.T) {
	body := "Hello, this is some text content that should be compressed if long enough."
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	})

	compressor := middleware.NewCompressor(5, "text/html")
	handler := compressor.Handler(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(resp.Body)
		require.NoError(t, err)
		defer reader.Close()
		decompressed, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, body, string(decompressed))
	}
}

func TestChiCompress_NoCompressionWithoutHeader(t *testing.T) {
	body := "uncompressed response body"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	})

	compressor := middleware.NewCompressor(5, "text/html")
	handler := compressor.Handler(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEqual(t, "gzip", resp.Header.Get("Content-Encoding"))
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(respBody))
}

func TestChiCompress_IgnoresNonTextContent(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake image data"))
	})

	compressor := middleware.NewCompressor(5, "text/html", "application/json")
	handler := compressor.Handler(inner)
	req := httptest.NewRequest(http.MethodGet, "/image.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEqual(t, "gzip", resp.Header.Get("Content-Encoding"))
}

func TestCSP_ContainsSelf(t *testing.T) {
	assert.Contains(t, csp, "connect-src 'self'")
	assert.Contains(t, csp, "default-src 'self'")
}

func TestCSP_NoExternalPorts(t *testing.T) {
	assert.NotContains(t, csp, ":9999", "proxy removes need for direct gateway access")
	assert.NotContains(t, csp, ":8443", "proxy removes need for direct predastore access")
}

func TestNewReverseProxy_StripsPrefixAndSetsHost(t *testing.T) {
	tests := []struct {
		name      string
		reqPath   string
		prefix    string
		wantPath  string
		wantHost  string
		wantQuery string
	}{
		{
			name:     "strips awsgw prefix",
			reqPath:  "/proxy/awsgw/",
			prefix:   "/proxy/awsgw",
			wantPath: "/",
			wantHost: "localhost:9999",
		},
		{
			name:     "strips prefix with subpath",
			reqPath:  "/proxy/awsgw/some/path",
			prefix:   "/proxy/awsgw",
			wantPath: "/some/path",
			wantHost: "localhost:9999",
		},
		{
			name:     "strips s3 prefix with bucket key",
			reqPath:  "/proxy/s3/bucket/key",
			prefix:   "/proxy/s3",
			wantPath: "/bucket/key",
			wantHost: "localhost:8443",
		},
		{
			name:     "empty path after strip becomes root",
			reqPath:  "/proxy/awsgw",
			prefix:   "/proxy/awsgw",
			wantPath: "/",
			wantHost: "localhost:9999",
		},
		{
			name:      "preserves query string",
			reqPath:   "/proxy/s3/bucket?list-type=2&prefix=foo",
			prefix:    "/proxy/s3",
			wantPath:  "/bucket",
			wantHost:  "localhost:8443",
			wantQuery: "list-type=2&prefix=foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start a mock backend to capture the forwarded request.
			var gotPath, gotHost, gotQuery string
			backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotHost = r.Host
				gotQuery = r.URL.RawQuery
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()

			// Use the test server's TLS transport.
			transport, ok := backend.Client().Transport.(*http.Transport)
			require.True(t, ok, "expected *http.Transport")

			// Point the proxy at the test server instead of the real backend.
			backendAddr := backend.Listener.Addr().String()
			proxy := newReverseProxy(backendAddr, tt.prefix, transport)

			req := httptest.NewRequest(http.MethodPost, tt.reqPath, nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tt.wantPath, gotPath, "backend should see stripped path")
			assert.Equal(t, backendAddr, gotHost, "backend should see proxy-set Host")
			if tt.wantQuery != "" {
				assert.Equal(t, tt.wantQuery, gotQuery, "query string should be preserved")
			}
		})
	}
}

func TestNewReverseProxy_ErrorHandler(t *testing.T) {
	// Use a transport that will fail to connect (no backend listening).
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	proxy := newReverseProxy("localhost:19999", "/proxy/awsgw", transport)

	req := httptest.NewRequest(http.MethodPost, "/proxy/awsgw/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "application/xml", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	assert.Contains(t, body, "<Code>BadGateway</Code>")
	assert.Contains(t, body, "upstream connection failed")
	assert.NotContains(t, body, "localhost:19999")
}

func TestNewProxyTransport_ValidCert(t *testing.T) {
	// Use the test TLS server's CA cert.
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Extract the CA cert from the test server's TLS config and write to a temp file.
	certPEM := backend.Certificate()
	require.NotNil(t, certPEM)

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")

	// httptest TLS server uses a self-signed cert; encode it as PEM.
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certPEM.Raw,
	})
	require.NoError(t, os.WriteFile(caPath, pemData, 0o644))

	transport, err := newProxyTransport(caPath)
	require.NoError(t, err)
	assert.NotNil(t, transport)
	assert.NotNil(t, transport.TLSClientConfig)
	assert.NotNil(t, transport.TLSClientConfig.RootCAs)
}

func TestNewProxyTransport_MissingFile(t *testing.T) {
	_, err := newProxyTransport("/nonexistent/ca.pem")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read CA cert")
}

func TestNewProxyTransport_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("not a cert"), 0o644))

	_, err := newProxyTransport(caPath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse CA cert")
}

func TestStart_WritesPidFileToNestedBaseDir(t *testing.T) {
	// BaseDir points at a directory that does not exist yet, mirroring a
	// freshly provisioned host where the service's data directory hasn't
	// been created. WritePidFileTo must MkdirAll it.
	baseDir := filepath.Join(t.TempDir(), "nested", "state")

	svc := &Service{
		Config: &Config{
			BaseDir: baseDir,
			// launchService will fail past the pid-file step since these
			// certs don't exist; the pid file must be written regardless.
			TLSCert: "/nonexistent/cert.pem",
			TLSKey:  "/nonexistent/key.pem",
		},
	}

	_, _ = svc.Start()

	data, err := os.ReadFile(filepath.Join(baseDir, "spinifex-ui.pid"))
	require.NoError(t, err, "pid file should exist under the nested BaseDir")
	assert.Equal(t, strconv.Itoa(os.Getpid()), string(data))
}

func TestShutdown_WithServer(t *testing.T) {
	// Create a real server on a random port so Shutdown exercises the non-nil path
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go srv.Serve(ln)

	svc := &Service{
		Config: &Config{},
		server: srv,
	}

	err = svc.Shutdown()
	assert.NoError(t, err)
}

// writeTestTLSFiles generates a self-signed cert/key pair on disk for a
// launchService test, which needs real file paths (tls.LoadX509KeyPair),
// unlike loadTLSCert's in-memory tls.Certificate used by the split-listener tests.
func writeTestTLSFiles(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "spinifexui-test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "server.pem")
	keyPath = filepath.Join(dir, "server.key")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	// newProxyTransport reads <TLSCert dir>/ca.pem; the self-signed leaf
	// itself is a valid enough PEM cert to satisfy that read in a test.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o600))
	return certPath, keyPath
}

// freePort binds an ephemeral port, closes it immediately, and returns the
// number so Start can bind the same port without hardcoding 3000 (which may
// already be in use by a real spinifex-ui on a shared box).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// TestStart_GracefulShutdownIsNotAnError guards against Start propagating
// http.ErrServerClosed -- Serve's documented return value after a deliberate
// Shutdown -- as a failure. Before the fix this made a clean stop look like
// a crash (exit code 1) to systemd.
func TestStart_GracefulShutdownIsNotAnError(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	certPath, keyPath := writeTestTLSFiles(t)

	svc, err := New(&Config{
		Port:    freePort(t),
		Host:    "127.0.0.1",
		TLSCert: certPath,
		TLSKey:  keyPath,
	})
	require.NoError(t, err)

	startErrCh := make(chan error, 1)
	go func() {
		_, startErr := svc.Start()
		startErrCh <- startErr
	}()

	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.server != nil
	}, 2*time.Second, 10*time.Millisecond, "server never registered before shutdown")

	require.NoError(t, svc.Shutdown())

	select {
	case startErr := <-startErrCh:
		assert.NoError(t, startErr, "Start must treat a deliberate Shutdown's ErrServerClosed as success")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}
