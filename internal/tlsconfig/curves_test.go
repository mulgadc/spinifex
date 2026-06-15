package tlsconfig_test

import (
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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

func TestCurves_OnlyApprovedEntries(t *testing.T) {
	want := []tls.CurveID{
		tls.X25519MLKEM768,
		tls.SecP384r1MLKEM1024,
		tls.X25519,
		tls.CurveP384,
	}
	if !slices.Equal(tlsconfig.Curves, want) {
		t.Errorf("Curves = %v, want %v", tlsconfig.Curves, want)
	}
}

func TestCurves_ExcludesWeakPrimitives(t *testing.T) {
	forbidden := []tls.CurveID{
		tls.SecP256r1MLKEM768, // weakest ML-KEM hybrid (P-256 component)
		tls.CurveP256,         // not CNSA 2.0
		tls.CurveP521,         // niche, no current peers
	}
	for _, c := range forbidden {
		if slices.Contains(tlsconfig.Curves, c) {
			t.Errorf("Curves includes forbidden curve %v", c)
		}
	}
}

// TestIntegration_NegotiatesPQHybrid verifies that a Curves-configured server
// negotiates a PQ hybrid at TLS 1.3; checks for any approved hybrid to stay
// stable across Go stdlib reorderings.
func TestIntegration_NegotiatesPQHybrid(t *testing.T) {
	cert := selfSignedCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:          pool,
				CurvePreferences: tlsconfig.Curves,
			},
		},
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()

	state := resp.TLS
	if state == nil {
		t.Fatal("response has no TLS state")
	}
	if state.Version != tls.VersionTLS13 {
		t.Errorf("negotiated version = 0x%x, want TLS 1.3 (0x%x)", state.Version, tls.VersionTLS13)
	}
	pqHybrids := []tls.CurveID{tls.X25519MLKEM768, tls.SecP384r1MLKEM1024}
	if !slices.Contains(pqHybrids, state.CurveID) {
		t.Errorf("negotiated curve = %v, want one of %v (PQ hybrid)", state.CurveID, pqHybrids)
	}
}

// TestIntegration_TLS12ClientRejected confirms MinVersion TLS 1.3 is enforced:
// a TLS 1.2-pinned client must get a handshake error, not a silent downgrade.
func TestIntegration_TLS12ClientRejected(t *testing.T) {
	cert := selfSignedCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
				MaxVersion: tls.VersionTLS12,
			},
		},
	}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("TLS 1.2-pinned client succeeded; MinVersion not enforced")
	}
	if !strings.Contains(err.Error(), "protocol version") &&
		!strings.Contains(err.Error(), "tls:") {
		t.Errorf("expected protocol-version handshake error, got: %v", err)
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tlsconfig-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return pair
}
