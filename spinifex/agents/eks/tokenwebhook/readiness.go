package tokenwebhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
)

const (
	readinessTimeout = 2 * time.Second
	readinessTTL     = 2 * time.Second
)

// gatewayProbe reports whether the gateway this webhook relays to accepts a TLS
// handshake. It sends no credentials and caches the result so frequent load
// balancer probes never reach the gateway more than once per ttl.
type gatewayProbe struct {
	addr    string
	tlsCfg  *tls.Config
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu      sync.Mutex
	checked time.Time
	ok      bool
}

func newGatewayProbe(gatewayURL, caPath string) (*gatewayProbe, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return nil, fmt.Errorf("parse gateway URL %q: %w", gatewayURL, err)
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return nil, fmt.Errorf("gateway URL %q has no host", gatewayURL)
	}
	if port == "" {
		port = "443"
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, CurvePreferences: tlsconfig.Curves}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway CA %q has no usable certificates", caPath)
		}
		cfg.RootCAs = pool
	}
	return &gatewayProbe{
		addr:    net.JoinHostPort(host, port),
		tlsCfg:  cfg,
		timeout: readinessTimeout,
		ttl:     readinessTTL,
		now:     time.Now,
	}, nil
}

// ready returns the cached result while it is younger than ttl, else probes.
// Concurrent callers wait for one probe and share its result.
func (p *gatewayProbe) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.checked.IsZero() && p.now().Sub(p.checked) < p.ttl {
		return p.ok
	}
	p.ok = p.probe()
	p.checked = p.now()
	return p.ok
}

func (p *gatewayProbe) probe() bool {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: p.tlsCfg}
	conn, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// handle serves 200 when the gateway is reachable and 503 when it is not.
func (p *gatewayProbe) handle(w http.ResponseWriter, _ *http.Request) {
	if p.ready() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	http.Error(w, "gateway unreachable", http.StatusServiceUnavailable)
}
