package spinifexui

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// newProxyTransport creates an *http.Transport that trusts the given CA
// certificate so the reverse proxy can connect to backend services using
// self-signed TLS certificates.
func newProxyTransport(caCertPath string) (*http.Transport, error) {
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %s: %w", caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert from %s", caCertPath)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: pool,
		},
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}, nil
}

// newReverseProxy forwards requests to backendHost after stripping pathPrefix.
// Sets req.Host to the backend so SigV4 canonical-host verification passes.
// The transport is wrapped with otelhttp so proxied requests inject
// traceparent, letting a UI-initiated trace continue into awsgw/predastore
// instead of stopping at spinifex-ui.
func newReverseProxy(backendHost, pathPrefix string, transport http.RoundTripper) http.Handler {
	target := &url.URL{
		Scheme: "https",
		Host:   backendHost,
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host

			// Strip the proxy path prefix.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, pathPrefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
		},
		Transport: otelhttp.NewTransport(transport),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("Proxy error", "backend", backendHost, "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<Error><Code>BadGateway</Code>`+
				`<Message>upstream connection failed</Message></Error>`)
		},
	}

	return proxy
}
