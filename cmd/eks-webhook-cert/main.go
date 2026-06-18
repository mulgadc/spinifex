// Command eks-webhook-cert mints, then reuses, a self-signed ECDSA-P256 serving
// certificate for an in-cluster admission webhook and prints its base64 PEM so
// the addon renderer can embed it as the webhook Secret + each caBundle.
//
// The eks-node AMI carries no openssl, so this is the Go equivalent of the
// upstream Helm chart's install-time genSelfSignedCert: one cert per cluster,
// cached on disk under -dir, stable across addon-sync ticks. The single cert is
// its own CA (IsCA + ServerAuth), so the same PEM serves as both the TLS leaf
// and the caBundle the apiserver trusts.
//
// Usage:
//
//	eks-webhook-cert -dir /var/lib/spinifex-eks/webhook-certs/<addon> \
//	    -cn <svc>.<ns>.svc -dns <svc>.<ns>.svc,<svc>.<ns>.svc.cluster.local
//
// Output (one line): <ca_b64>\t<tls_crt_b64>\t<tls_key_b64>
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	dir := flag.String("dir", "", "directory to cache tls.crt/tls.key in")
	cn := flag.String("cn", "", "certificate common name")
	dnsCSV := flag.String("dns", "", "comma-separated DNS SANs")
	flag.Parse()

	if *dir == "" || *cn == "" || *dnsCSV == "" {
		fatal("eks-webhook-cert: -dir, -cn and -dns are required")
	}
	dnsNames := splitCSV(*dnsCSV)
	if len(dnsNames) == 0 {
		fatal("eks-webhook-cert: -dns lists no names")
	}

	certPEM, keyPEM, err := ensureCert(*dir, *cn, dnsNames)
	if err != nil {
		fatal("eks-webhook-cert: " + err.Error())
	}
	b := base64.StdEncoding.EncodeToString
	fmt.Printf("%s\t%s\t%s\n", b(certPEM), b(certPEM), b(keyPEM))
}

// ensureCert returns the cached cert/key under dir, minting and persisting a
// fresh self-signed pair (for cn + dnsNames) when none is present or readable.
func ensureCert(dir, cn string, dnsNames []string) (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if c, cerr := os.ReadFile(certPath); cerr == nil {
		if k, kerr := os.ReadFile(keyPath); kerr == nil {
			if _, perr := x509.ParseCertificate(decodeFirst(c)); perr == nil && len(k) > 0 {
				return c, k, nil
			}
		}
	}

	certPEM, keyPEM, err = generateSelfSigned(cn, dnsNames)
	if err != nil {
		return nil, nil, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err = os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, fmt.Errorf("write %s: %w", certPath, err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write %s: %w", keyPath, err)
	}
	return certPEM, keyPEM, nil
}

// generateSelfSigned mints an ECDSA-P256 self-signed cert valid as both a CA and
// a server cert, with the given common name and DNS SANs.
func generateSelfSigned(cn string, dnsNames []string) (certPEM, keyPEM []byte, err error) {
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
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
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

// decodeFirst returns the DER bytes of the first PEM block, or nil.
func decodeFirst(pemBytes []byte) []byte {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil
	}
	return blk.Bytes
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}
