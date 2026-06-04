package main

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

	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

// tlsCertificate is the webhook's loopback serving certificate.
type tlsCertificate struct {
	cert tls.Certificate
}

func (c tlsCertificate) serverTLSConfig() *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{c.cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
}

// ensureServingCert loads the persisted self-signed serving cert/key, or mints
// a fresh ECDSA-P256 pair (SAN 127.0.0.1 / localhost) and persists it. Reusing
// across restarts keeps the CA the apiserver loaded into its webhook kubeconfig
// valid even if the webhook process is restarted. Returns the parsed cert and
// the certificate PEM (for embedding as the kubeconfig CA bundle).
func ensureServingCert(certPath, keyPath string) (tlsCertificate, []byte, error) {
	if certPEM, keyPEM, ok := readCertPair(certPath, keyPath); ok {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err == nil {
			return tlsCertificate{cert: cert}, certPEM, nil
		}
		// Fall through to regenerate on a corrupt/incompatible persisted pair.
	}

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return tlsCertificate{}, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tlsCertificate{}, nil, fmt.Errorf("write cert %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tlsCertificate{}, nil, fmt.Errorf("write key %s: %w", keyPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tlsCertificate{}, nil, fmt.Errorf("parse minted keypair: %w", err)
	}
	return tlsCertificate{cert: cert}, certPEM, nil
}

func readCertPair(certPath, keyPath string) (certPEM, keyPEM []byte, ok bool) {
	c, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, false
	}
	k, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, false
	}
	return c, k, true
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
