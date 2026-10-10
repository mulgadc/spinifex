package tokenwebhook

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/mulgadc/bluebottle/pkg/tlsconfig"
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

	certPEM, keyPEM, err := generateSelfSigned()
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

func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("serial: %w", err)
	}
	now := time.Now().UTC()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "eks-token-webhook"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
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
