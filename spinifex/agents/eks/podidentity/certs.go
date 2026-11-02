package podidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/konnectivitycert"
)

const (
	// ClientCN is the identity the webhook presents to the API server; the
	// ClusterRoleBinding names this user.
	ClientCN = "spinifex:eks-pod-identity-webhook"

	servingCertFile = "serving.crt"
	servingKeyFile  = "serving.key"
	clientCertFile  = "client.crt"
	clientKeyFile   = "client.key"
	certRenewWindow = 30 * 24 * time.Hour
	certValidity    = 365 * 24 * time.Hour
)

// CertConfig names the k3s CAs that sign the webhook's two certificates and
// the directory they are cached in.
type CertConfig struct {
	Dir          string
	ServerCACert string
	ServerCAKey  string
	ClientCACert string
	ClientCAKey  string
}

// Certs are the paths of the cached certificate and key pairs.
type Certs struct {
	ServingCert, ServingKey string
	ClientCert, ClientKey   string
}

// EnsureCerts mints, or reuses while still valid and signed by the current CA,
// a serving certificate for 127.0.0.1 signed by the k3s server CA and a client
// certificate signed by the k3s client CA. Keys are written 0600.
func EnsureCerts(c CertConfig) (Certs, error) {
	out := Certs{
		ServingCert: filepath.Join(c.Dir, servingCertFile), ServingKey: filepath.Join(c.Dir, servingKeyFile),
		ClientCert: filepath.Join(c.Dir, clientCertFile), ClientKey: filepath.Join(c.Dir, clientKeyFile),
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return Certs{}, fmt.Errorf("mkdir %s: %w", c.Dir, err)
	}
	serving := leafSpec{cn: "pod-identity-webhook", usage: x509.ExtKeyUsageServerAuth, ips: []net.IP{net.IPv4(127, 0, 0, 1)}}
	if err := ensureLeaf(out.ServingCert, out.ServingKey, c.ServerCACert, c.ServerCAKey, serving); err != nil {
		return Certs{}, fmt.Errorf("serving certificate: %w", err)
	}
	client := leafSpec{cn: ClientCN, usage: x509.ExtKeyUsageClientAuth}
	if err := ensureLeaf(out.ClientCert, out.ClientKey, c.ClientCACert, c.ClientCAKey, client); err != nil {
		return Certs{}, fmt.Errorf("client certificate: %w", err)
	}
	return out, nil
}

type leafSpec struct {
	cn    string
	usage x509.ExtKeyUsage
	ips   []net.IP
}

func ensureLeaf(certPath, keyPath, caCertPath, caKeyPath string, spec leafSpec) error {
	ca, caKey, err := konnectivitycert.LoadCA(caCertPath, caKeyPath)
	if err != nil {
		return err
	}
	if leafCurrent(certPath, keyPath, ca, spec) {
		return nil
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}
	now := time.Now().UTC()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: spec.cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{spec.usage},
		BasicConstraintsValid: true,
		IPAddresses:           spec.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	// Key first: a cert without its key is never published.
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// leafCurrent reports whether the cached pair loads, matches, was signed by ca,
// has the wanted usage and SANs, and is not near expiry.
func leafCurrent(certPath, keyPath string, ca *x509.Certificate, spec leafSpec) bool {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	crt, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || crt.CheckSignatureFrom(ca) != nil {
		return false
	}
	if inRenewWindow(crt, time.Now()) || crt.Subject.CommonName != spec.cn {
		return false
	}
	if len(crt.ExtKeyUsage) != 1 || crt.ExtKeyUsage[0] != spec.usage {
		return false
	}
	for _, ip := range spec.ips {
		found := false
		for _, have := range crt.IPAddresses {
			found = found || have.Equal(ip)
		}
		if !found {
			return false
		}
	}
	return true
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// inRenewWindow reports whether crt expires within the renew window at now.
func inRenewWindow(crt *x509.Certificate, now time.Time) bool {
	return crt.NotAfter.Sub(now) < certRenewWindow
}

// NeedsRenewal reports whether either cached certificate is inside its renew
// window at now. A running process exits when this is true so the supervisor
// restarts it and EnsureCerts reissues; an unreadable certificate also counts.
func NeedsRenewal(c Certs, now time.Time) bool {
	for _, path := range []string{c.ServingCert, c.ClientCert} {
		b, err := os.ReadFile(path)
		if err != nil {
			return true
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return true
		}
		crt, err := x509.ParseCertificate(blk.Bytes)
		if err != nil || inRenewWindow(crt, now) {
			return true
		}
	}
	return false
}
