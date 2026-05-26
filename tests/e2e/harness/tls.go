//go:build e2e

package harness

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// FetchServedCert opens a TLS connection to host:port and returns the
// leaf certificate the server presented. Trust is bypassed (the caller
// decides whether to validate against a CA pool afterwards).
func FetchServedCert(host string, port int, timeout time.Duration) (*x509.Certificate, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", fmt.Sprintf("%s:%d", host, port), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
	})
	if err != nil {
		return nil, fmt.Errorf("tls dial %s:%d: %w", host, port, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no peer certificates from %s:%d", host, port)
	}
	return certs[0], nil
}

// HTTPSGet probes url using the given CA pool (nil = system trust store).
// Returns the HTTP status code and body. Connection errors return status 0.
func HTTPSGet(url string, caPool *x509.CertPool, timeout time.Duration) (int, []byte, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caPool,
			},
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// FetchTLSPosture opens a TLS connection to host:port and returns the
// negotiated version and curve. Trust is bypassed because the goal is to
// observe the handshake, not validate the cert chain. Used by e2e tests to
// assert that deployed services actually negotiate TLS 1.3 + PQ hybrid
// (see internal/tlsconfig.Curves) under the production FIPS build.
func FetchTLSPosture(host string, port int, timeout time.Duration) (version uint16, curve tls.CurveID, err error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", fmt.Sprintf("%s:%d", host, port), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("tls dial %s:%d: %w", host, port, err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	return state.Version, state.CurveID, nil
}

// LoadCAPool reads a PEM-encoded CA bundle from path and returns an x509 pool.
func LoadCAPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("no certs parsed from %s", path)
	}
	return pool, nil
}
