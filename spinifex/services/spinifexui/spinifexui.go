package spinifexui

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

var serviceName = "spinifex-ui"

//go:embed all:frontend/dist
var distFS embed.FS

// Config holds the configuration for the spinifex-ui service.
type Config struct {
	Port    int    `json:"port"`
	Host    string `json:"host"`
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// BaseDir is the base directory for PID files and state.
	BaseDir string `json:"base_dir"`
	// Region is the cluster's AWS-parity region, served to the browser so the
	// console can sign requests for the cluster it is actually talking to.
	Region string `json:"region"`
}

// clusterConfig is the body of GET /api/config. It carries only non-secret
// facts, matching the unauthenticated posture of /api/ca.pem.
type clusterConfig struct {
	Region string `json:"region"`
}

// namedRoute labels a route's request metrics with a fixed action, so
// rpc_method carries the endpoint rather than the bare HTTP verb.
func namedRoute(action string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otelsetup.SetRequestAction(r.Context(), action)
		next.ServeHTTP(w, r)
	})
}

// clusterConfigHandler serves the facts the SPA needs before it can sign
// anything. no-store keeps a caching proxy in front of several clusters from
// serving one the other's region.
func clusterConfigHandler(region string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if region == "" {
			slog.Error("Cluster region is not configured; the console cannot sign requests")
			http.Error(w, "Cluster region is not configured", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(clusterConfig{Region: region}); err != nil {
			slog.Error("Failed to write cluster config", "error", err)
		}
	}
}

// Service represents the spinifex-ui service.
type Service struct {
	Config *Config
	server *http.Server
	mu     sync.Mutex
}

// New creates a new spinifex-ui service.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for spinifex-ui service")
	}

	// Set defaults
	if cfg.Port == 0 {
		cfg.Port = 3000
	}
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}

	// Default TLS paths: production layout (/etc/spinifex/) if it exists,
	// otherwise fall back to dev layout (~/spinifex/config/).
	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		if info, err := os.Stat("/etc/spinifex"); err == nil && info.IsDir() {
			if cfg.TLSCert == "" {
				cfg.TLSCert = "/etc/spinifex/server.pem"
			}
			if cfg.TLSKey == "" {
				cfg.TLSKey = "/etc/spinifex/server.key"
			}
		} else if homeDir, err := os.UserHomeDir(); err == nil {
			if cfg.TLSCert == "" {
				cfg.TLSCert = filepath.Join(homeDir, "spinifex", "config", "server.pem")
			}
			if cfg.TLSKey == "" {
				cfg.TLSKey = filepath.Join(homeDir, "spinifex", "config", "server.key")
			}
		}
	}

	return &Service{
		Config: cfg,
	}, nil
}

// Start starts the spinifex-ui service.
func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		slog.Error("Failed to write pid file", "err", err)
	}

	err := svc.launchService()
	if err != nil {
		return 0, err
	}

	return os.Getpid(), nil
}

// Stop stops the spinifex-ui service.
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BaseDir, serviceName)
}

// Status returns the status of the spinifex-ui service.
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BaseDir, serviceName)
}

// Shutdown gracefully shuts down the spinifex-ui service.
func (svc *Service) Shutdown() error {
	svc.mu.Lock()
	server := svc.server
	svc.mu.Unlock()

	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
	return svc.Stop()
}

// Reload reloads the spinifex-ui service configuration.
func (svc *Service) Reload() error {
	return nil
}

// launchService starts the HTTP server.
// newSPAHandler serves the embedded build, falling back to index.html so the
// router can resolve client-side paths.
func newSPAHandler(contentFS fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(contentFS))

	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		file, err := contentFS.Open(path)
		if err == nil {
			_ = file.Close()
			// Asset filenames are content-hashed, so the action names the branch
			// rather than the path — one series per build otherwise.
			otelsetup.SetRequestAction(r.Context(), "ui.static")
			w.Header().Set("Cache-Control", assetCacheControl(path))
			fileServer.ServeHTTP(w, r)
			return
		}

		otelsetup.SetRequestAction(r.Context(), "ui.spa")
		w.Header().Set("Cache-Control", "no-cache")
		indexContent, err := fs.ReadFile(contentFS, "index.html")
		if err != nil {
			http.Error(w, "index.html not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(indexContent); err != nil {
			slog.Error("Failed to write index.html response", "error", err)
		}
	}
}

// embed.FS reports a zero ModTime and FileServer sets no ETag, so a revalidated
// response has no validator to answer 304 with. The content hash under assets/
// is that validator: the name addresses one build, so it can be cached forever.
// Everything else is revalidated, index.html above all — it is what points at
// the current build's hashed names.
func assetCacheControl(path string) string {
	if strings.HasPrefix(path, "assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func (svc *Service) launchService() error {
	// Strip the "frontend/dist" prefix from embedded filesystem
	contentFS, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		slog.Error("Failed to create sub filesystem", "error", err)
		return fmt.Errorf("failed to get embedded filesystem: %w", err)
	}

	// Check if certificates exist
	if _, err := os.Stat(svc.Config.TLSCert); os.IsNotExist(err) {
		slog.Error("Certificate file not found", "path", svc.Config.TLSCert)
		return fmt.Errorf("certificate file not found: %s", svc.Config.TLSCert)
	}
	if _, err := os.Stat(svc.Config.TLSKey); os.IsNotExist(err) {
		slog.Error("Key file not found", "path", svc.Config.TLSKey)
		return fmt.Errorf("key file not found: %s", svc.Config.TLSKey)
	}

	// Derive CA cert path from server cert directory.
	caCertPath := filepath.Join(filepath.Dir(svc.Config.TLSCert), "ca.pem")

	// Build TLS transport for reverse proxies using the same CA the UI trusts.
	proxyTransport, err := newProxyTransport(caCertPath)
	if err != nil {
		return fmt.Errorf("proxy transport: %w", err)
	}

	spaHandler := newSPAHandler(contentFS)

	mux := http.NewServeMux()

	// Reverse proxy routes — must be registered before the SPA catch-all.
	mux.Handle("/proxy/awsgw/", namedRoute("ui.proxy.awsgw", newReverseProxy("localhost:9999", "/proxy/awsgw", proxyTransport)))
	mux.Handle("/proxy/s3/", namedRoute("ui.proxy.s3", newReverseProxy("localhost:8443", "/proxy/s3", proxyTransport)))

	// CA certificate download.
	mux.HandleFunc("/api/ca.pem", func(w http.ResponseWriter, r *http.Request) {
		otelsetup.SetRequestAction(r.Context(), "ui.api.ca-cert")
		if _, err := os.Stat(caCertPath); err != nil {
			if os.IsNotExist(err) {
				slog.Warn("CA certificate requested but not found", "path", caCertPath)
				http.Error(w, "CA certificate not yet generated. Run 'spx admin init' to create it.", http.StatusNotFound)
			} else {
				slog.Error("CA certificate stat failed", "path", caCertPath, "error", err)
				http.Error(w, "Unable to read CA certificate", http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="spinifex-ca.pem"`)
		http.ServeFile(w, r, caCertPath)
	})

	mux.Handle("/api/config", namedRoute("ui.api.config", clusterConfigHandler(svc.Config.Region)))

	// SPA catch-all.
	mux.Handle("/", spaHandler)

	// Chi's Compress middleware handles http.Flusher passthrough for streaming
	// proxy responses and skips already-compressed content types.
	compressor := middleware.NewCompressor(5, "text/html", "text/css",
		"application/javascript", "text/javascript", "application/json",
		"image/svg+xml", "text/plain")
	traced := otelsetup.HTTPMiddleware("spinifex-ui")(mux)
	finalHandler := securityHeadersMiddleware(compressor.Handler(traced))

	addr := fmt.Sprintf("%s:%d", svc.Config.Host, svc.Config.Port)

	server := &http.Server{
		Addr:              addr,
		Handler:           finalHandler,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	svc.mu.Lock()
	svc.server = server
	svc.mu.Unlock()

	// Setup graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		slog.Info("Received shutdown signal, gracefully shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("Failed to shutdown server gracefully", "err", err)
		}
	}()

	// Listen on the port and detect TLS vs plain HTTP on the same port.
	// Plain HTTP connections get a 301 redirect to HTTPS instead of an error.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	cert, err := tls.LoadX509KeyPair(svc.Config.TLSCert, svc.Config.TLSKey)
	if err != nil {
		return fmt.Errorf("load TLS keypair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}

	splitLn := &tlsSplitListener{
		Listener: ln,
		port:     svc.Config.Port,
		tlsCfg:   tlsConfig,
	}

	slog.Info("Starting spinifex-ui service with HTTPS (auto-redirect HTTP)", "addr", addr)
	// ErrServerClosed is Serve's documented return value after Shutdown was
	// called deliberately (see the SIGTERM handler above) -- success, not a
	// failure to report or exit non-zero for.
	if err := server.Serve(splitLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Content-Security-Policy header. All API requests are proxied through the
// same origin so connect-src only needs 'self'.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; font-src 'self' data:; connect-src 'self'; " +
	"object-src 'none'; base-uri 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; upgrade-insecure-requests;"

func securityHeadersMiddleware(next http.Handler) http.Handler {
	slog.Info("Content-Security-Policy configured", "csp", csp)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), browsing-topics=()")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
