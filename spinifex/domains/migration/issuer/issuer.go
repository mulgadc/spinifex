package issuer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/pki"
)

const (
	// CertLifetime is the validity of a migration client certificate.
	CertLifetime = time.Hour
	// notBeforeSkew absorbs clock difference between this node and AWS.
	notBeforeSkew = time.Minute
)

// ClientCertificate is a migration certificate with its in-memory key.
type ClientCertificate struct {
	Certificate *x509.Certificate
	key         *ecdsa.PrivateKey
}

// MarshalJSON refuses, so the key cannot reach a record or a log line.
func (c *ClientCertificate) MarshalJSON() ([]byte, error) {
	return nil, errors.New("migration client certificate must not be serialised")
}

// LogValue keeps the key out of logs.
func (c *ClientCertificate) LogValue() slog.Value {
	if c == nil || c.Certificate == nil {
		return slog.StringValue("<nil>")
	}
	return slog.GroupValue(slog.String("serial", c.Certificate.SerialNumber.String()))
}

// String never prints the key.
func (c *ClientCertificate) String() string { return "ClientCertificate{redacted}" }

// GoString never prints the key.
func (c *ClientCertificate) GoString() string { return c.String() }

// CertIssuer issues migration client certificates.
type CertIssuer interface {
	Issue(accountID, connectionID string) (*ClientCertificate, error)
}

// Issuer signs migration certificates with the platform CA. It has no way to
// issue any other certificate form.
type Issuer struct {
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	clusterID string
	now       func() time.Time
}

var _ CertIssuer = (*Issuer)(nil)

// New builds an Issuer from a loaded CA. now is the clock.
func New(caCert *x509.Certificate, caKey *rsa.PrivateKey, clusterID string, now func() time.Time) (*Issuer, error) {
	if caCert == nil || caKey == nil {
		return nil, fmt.Errorf("migration issuer: CA certificate and key required")
	}
	if !caCert.IsCA || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("migration issuer: certificate is not a CA")
	}
	pub, ok := caCert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&caKey.PublicKey) {
		return nil, fmt.Errorf("migration issuer: CA key does not match CA certificate")
	}
	if err := (Subject{ClusterID: clusterID, AccountID: "000000000000", ConnectionID: "x"}).Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{caCert: caCert, caKey: caKey, clusterID: clusterID, now: now}, nil
}

// Load builds an Issuer from the platform CA files.
func Load(caCertPath, caKeyPath, clusterID string, now func() time.Time) (*Issuer, error) {
	cert, key, err := pki.LoadCAKeyPair(caCertPath, caKeyPath)
	if err != nil {
		return nil, err
	}
	return New(cert, key, clusterID, now)
}

// Issue makes a one-hour ECDSA P-256 certificate for one connection. The
// subject is built here from the account and connection; callers cannot set
// any other field.
func (i *Issuer) Issue(accountID, connectionID string) (*ClientCertificate, error) {
	name, err := Subject{ClusterID: i.clusterID, AccountID: accountID, ConnectionID: connectionID}.Name()
	if err != nil {
		return nil, err
	}
	now := i.now()
	if now.Before(i.caCert.NotBefore) || !now.Before(i.caCert.NotAfter) {
		return nil, fmt.Errorf("migration issuer: CA certificate is not valid now")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("migration issuer: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("migration issuer: serial: %w", err)
	}
	serial.Add(serial, big.NewInt(1))
	notAfter := now.Add(CertLifetime)
	if notAfter.After(i.caCert.NotAfter) {
		notAfter = i.caCert.NotAfter
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               name,
		NotBefore:             now.Add(-notBeforeSkew),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, i.caCert, &key.PublicKey, i.caKey)
	if err != nil {
		return nil, fmt.Errorf("migration issuer: sign: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("migration issuer: parse: %w", err)
	}
	return &ClientCertificate{Certificate: cert, key: key}, nil
}
