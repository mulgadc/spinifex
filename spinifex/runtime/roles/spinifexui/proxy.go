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

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
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
			RootCAs:          pool,
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: tlsconfig.Curves,
		},
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}, nil
}

// transferDeadline bounds one request through the S3 proxy. The server's
// ReadTimeout and WriteTimeout suit API calls, but would cut off an object
// upload or download on any link too slow to finish within them.
const transferDeadline = time.Hour

// withTransferDeadline replaces the server's read and write deadlines for
// each request with one transferDeadline away.
func withTransferDeadline(deadline time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		until := time.Now().Add(deadline)
		if err := rc.SetReadDeadline(until); err != nil {
			slog.Warn("Could not extend the transfer read deadline", "path", r.URL.Path, "error", err)
		}
		if err := rc.SetWriteDeadline(until); err != nil {
			slog.Warn("Could not extend the transfer write deadline", "path", r.URL.Path, "error", err)
		}
		next.ServeHTTP(w, r)
	})
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

			// The backend trusts X-Real-IP from loopback, which this hop always is.
			// The UI also listens publicly, so re-derive it rather than forward a
			// client's own claim. X-Request-ID passes through unchanged.
			pr.Out.Header.Set("X-Real-IP", ingresshttp.RequestClientIP(pr.In))

			// Strip the prefix from both path forms. RawPath must survive: a
			// client that percent-encodes a segment (a ':' in a bedrock
			// modelId) signs it, so dropping it re-encodes and breaks SigV4.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, pathPrefix)
			if pr.In.URL.RawPath != "" {
				pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, pathPrefix)
			}
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
