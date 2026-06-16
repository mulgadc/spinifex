//go:build e2e

package harness

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ResolveCACert walks the standard config locations for the Spinifex CA cert
// (matching the bash resolve_ca_cert helper) and returns the first hit.
// SPINIFEX_CA_CERT overrides the search — useful for runner-resident scenarios
// that SCP the CA off the cluster to a tmp path.
func ResolveCACert(env *Env) (string, error) {
	if explicit := os.Getenv("SPINIFEX_CA_CERT"); explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("SPINIFEX_CA_CERT=%s not readable", explicit)
	}
	candidates := []string{
		filepath.Join(env.ConfigDir, "ca.pem"),
		"/etc/spinifex/ca.pem",
		os.ExpandEnv("$HOME/spinifex/config/ca.pem"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("CA cert not found in any candidate location: %v", candidates)
}

// SystemCAPath is the canonical install location used by setup.sh.
const SystemCAPath = "/usr/local/share/ca-certificates/spinifex-ca.crt"

// ServerCertPath returns the local file path for a node's server cert,
// or empty string if the cert must be fetched over the wire instead.
func ServerCertPath(env *Env) string {
	if env.Mode == ModeSingle {
		return filepath.Join(env.ConfigDir, "server.pem")
	}
	return ""
}

// ParseCertFile reads a PEM cert from disk and returns the parsed x509.
func ParseCertFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseCertPEM(raw)
}

// ParseCertPEM parses the first CERTIFICATE block in raw.
func ParseCertPEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// CertHasIPSAN returns true if cert has ip in its IP SANs.
func CertHasIPSAN(cert *x509.Certificate, ip string) bool {
	target := net.ParseIP(ip)
	if target == nil {
		return false
	}
	for _, san := range cert.IPAddresses {
		if san.Equal(target) {
			return true
		}
	}
	return false
}

// CertHasDNSSAN returns true if cert has name (case-insensitive) in its DNS SANs.
func CertHasDNSSAN(cert *x509.Certificate, name string) bool {
	want := strings.ToLower(name)
	for _, san := range cert.DNSNames {
		if strings.ToLower(san) == want {
			return true
		}
	}
	return false
}

// OpenSSLVerify shells out to `openssl s_client -verify_return_error` against
// host:port using caFile as the trust anchor. Returns nil if verify code = 0.
// Kept as a shell-out (not crypto/tls) so we exercise the same code path the
// AWS SDK clients use in production tooling.
func OpenSSLVerify(t *testing.T, caFile, host string, port int) error {
	t.Helper()
	target := fmt.Sprintf("%s:%d", host, port)
	cmd := exec.Command("openssl", "s_client",
		"-CAfile", caFile,
		"-connect", target,
		"-verify_return_error",
		"-servername", host,
	)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(string(out), "Verify return code: 0") {
		return nil
	}
	return fmt.Errorf("openssl verify %s failed: %v\noutput:\n%s", target, err, out)
}

// GenerateSelfSignedCertPEM mints an ECDSA P-256 self-signed leaf for host
// (an IP or DNS name) and returns the cert PEM, the PKCS#8 key PEM, and the
// parsed leaf. Used by the ACM ImportCertificate → HTTPS listener e2e to feed
// a real keypair through the import → HAProxy termination path.
func GenerateSelfSignedCertPEM(host string) (certPEM, keyPEM []byte, leaf *x509.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse leaf: %w", err)
	}
	return certPEM, keyPEM, leaf, nil
}

// FingerprintMatches returns true if the two PEM certs share the SHA-256
// fingerprint that openssl x509 -fingerprint prints.
func FingerprintMatches(a, b *x509.Certificate) bool {
	if a == nil || b == nil {
		return false
	}
	return string(a.Raw) == string(b.Raw)
}
