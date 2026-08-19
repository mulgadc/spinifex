package handlers_rds

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// The lifetime is deliberately short: the cert is re-minted on every bootstrap
// fetch and never persisted, so a leaked key ages out on its own.
const (
	defaultServingCertKeyBits = 2048
	servingCertLifetime       = 90 * 24 * time.Hour
)

// Injected so tests mint from a throwaway CA, and so the file-backed
// implementation stays the only thing needing read access to ca.key.
type CALoader func() (*x509.Certificate, *rsa.PrivateKey, error)

// DNSName is empty on deployments without northstar, where the endpoint is the
// bare ENI IP — which is why the IP SAN is the required one.
type ServingCertRequest struct {
	DBInstanceIdentifier string
	PrivateIP            string
	DNSName              string
	// Zero takes defaultServingCertKeyBits. Only tests set it, to buy back the
	// keygen a 2048-bit key costs on every mint.
	KeyBits int
}

// PEM-encoded, in memory only.
type ServingCert struct {
	CertificatePEM string
	PrivateKeyPEM  string
}

// Unlike admin.GenerateSignedCert this writes no files and adds no SANs of its
// own, which would name the host rather than the database.
func MintServingCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, req ServingCertRequest) (*ServingCert, error) {
	if caCert == nil || caKey == nil {
		return nil, errors.New("rds serving cert: nil CA keypair")
	}
	if req.DBInstanceIdentifier == "" {
		return nil, errors.New("rds serving cert: DB instance identifier required")
	}
	ip := net.ParseIP(req.PrivateIP)
	if ip == nil {
		return nil, fmt.Errorf("rds serving cert: invalid private IP %q", req.PrivateIP)
	}

	bits := req.KeyBits
	if bits <= 0 {
		bits = defaultServingCertKeyBits
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, fmt.Errorf("rds serving cert: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("rds serving cert: serial: %w", err)
	}

	// The CN carries the DB identifier for operator legibility only; modern
	// clients match SANs, and verify-full has to work by IP as well as by name.
	notBefore := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: req.DBInstanceIdentifier, Organization: []string{"Spinifex RDS"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(servingCertLifetime),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{ip},
	}
	if req.DNSName != "" {
		tmpl.DNSNames = []string{req.DNSName}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("rds serving cert: sign: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("rds serving cert: marshal key: %w", err)
	}

	return &ServingCert{
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, nil
}

// Used to hand the agent the cluster CA it should trust alongside its own cert.
func EncodeCertPEM(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}
