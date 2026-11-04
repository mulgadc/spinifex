package tokenwebhook

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/agents/eks/konnectivitycert"
)

// serverTLSConfig is the webhook's loopback serving TLS config.
func serverTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
}

// ensureServingCert loads the persisted self-signed serving cert/key, or mints
// a fresh ECDSA-P256 pair (SAN 127.0.0.1 / localhost) and persists it. Reusing
// across restarts keeps the CA the apiserver loaded into its webhook kubeconfig
// valid even if the webhook process is restarted. Returns the parsed cert and
// the certificate PEM (for embedding as the kubeconfig CA bundle).
func ensureServingCert(certPath, keyPath string) (tls.Certificate, []byte, error) {
	// Fall through to regenerate on a missing/corrupt/incompatible persisted
	// pair. The cert PEM is re-read because tls.Certificate keeps only DER.
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if certPEM, err := os.ReadFile(certPath); err == nil {
			return cert, certPEM, nil
		}
	}

	now := time.Now().UTC()
	certPEM, keyPEM, err := konnectivitycert.Mint(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "eks-token-webhook"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}, nil, nil)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // G306: public certificate; the private key is written 0600 below.
		return tls.Certificate{}, nil, fmt.Errorf("write cert %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("write key %s: %w", keyPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse minted keypair: %w", err)
	}
	return cert, certPEM, nil
}

// writeAPIServerKubeconfig writes the kubeconfig kube-apiserver loads via
// --authentication-token-webhook-config-file. It points the apiserver at the
// loopback webhook and trusts its self-signed serving cert as the CA bundle.
func writeAPIServerKubeconfig(path, addr string, certPEM []byte) error {
	caB64 := base64.StdEncoding.EncodeToString(certPEM)
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: mulga-eks-token-webhook
  cluster:
    certificate-authority-data: %s
    server: https://%s/authenticate
users:
- name: kube-apiserver
contexts:
- name: webhook
  context:
    cluster: mulga-eks-token-webhook
    user: kube-apiserver
current-context: webhook
`, caB64, addr)
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
