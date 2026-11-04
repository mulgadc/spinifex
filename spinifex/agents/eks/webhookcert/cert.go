// Package webhookcert implements eks-webhook-cert, which mints, then reuses, a
// self-signed ECDSA-P256 serving certificate for an in-cluster admission webhook
// and prints its base64 PEM so the addon renderer can embed it as the webhook
// Secret + each caBundle.
//
// The eks-node AMI carries no openssl, so this is the Go equivalent of the
// upstream Helm chart's install-time genSelfSignedCert: one cert per cluster,
// cached on disk under -dir, stable across addon-sync ticks. The single cert is
// its own CA (IsCA + ServerAuth), so the same PEM serves as both the TLS leaf
// and the caBundle the apiserver trusts.
//
// Output (one line): <ca_b64>\t<tls_crt_b64>\t<tls_key_b64>
package webhookcert

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/konnectivitycert"
)

// Config names the webhook certificate and where it is cached.
type Config struct {
	Dir string
	CN  string
	// DNS is the comma-separated DNS SAN list.
	DNS string
	// Out receives the base64 PEM line.
	Out io.Writer
}

// Run mints or reuses the certificate cached under cfg.Dir and writes
// <ca_b64>\t<tls_crt_b64>\t<tls_key_b64> to cfg.Out.
func Run(_ context.Context, cfg Config) error {
	if cfg.Dir == "" || cfg.CN == "" || cfg.DNS == "" {
		return errors.New("-dir, -cn and -dns are required")
	}
	dnsNames := splitCSV(cfg.DNS)
	if len(dnsNames) == 0 {
		return errors.New("-dns lists no names")
	}

	certPEM, keyPEM, err := ensureCert(cfg.Dir, cfg.CN, dnsNames)
	if err != nil {
		return err
	}
	b := base64.StdEncoding.EncodeToString
	fmt.Fprintf(cfg.Out, "%s\t%s\t%s\n", b(certPEM), b(certPEM), b(keyPEM))
	return nil
}

// ensureCert returns the cached cert/key under dir, minting and persisting a
// fresh self-signed pair (for cn + dnsNames) when none is present or readable.
func ensureCert(dir, cn string, dnsNames []string) (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if c, cerr := os.ReadFile(certPath); cerr == nil {
		if k, kerr := os.ReadFile(keyPath); kerr == nil {
			if blk, _ := pem.Decode(c); blk != nil && len(k) > 0 {
				if _, perr := x509.ParseCertificate(blk.Bytes); perr == nil {
					return c, k, nil
				}
			}
		}
	}

	now := time.Now().UTC()
	certPEM, keyPEM, err = konnectivitycert.Mint(&x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
	}, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err = os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // G306: public certificate; the private key is written 0600 below.
		return nil, nil, fmt.Errorf("write %s: %w", certPath, err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, fmt.Errorf("write %s: %w", keyPath, err)
	}
	return certPEM, keyPEM, nil
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
