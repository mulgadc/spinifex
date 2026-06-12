package admin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IPSec peer-cert layout under configDir. ovs-monitor-ipsec consumes these
// paths via Open_vSwitch.other_config (certificate, private_key, ca_cert)
// set by the daemon at startup when network.ipsec_enabled is true.
const (
	ipsecDirName       = "ipsec"
	ipsecCertFileName  = "peer.pem"
	ipsecKeyFileName   = "peer.key"
	ipsecCACertSymlink = "ca.pem"
)

// id-kp-ipsecIKE per RFC 4945 §5.1.3.12. strongSwan accepts certs carrying
// this EKU as IKEv2 peer credentials; ovs-monitor-ipsec's auto-generated
// strongSwan config consumes them as-is.
var oidExtKeyUsageIPSecIKE = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 17}

// IPSecCertPaths returns the canonical cert/key paths for the per-node IPsec peer
// credential pair under {configDir}/ipsec/.
func IPSecCertPaths(configDir string) (certPath, keyPath string) {
	dir := filepath.Join(configDir, ipsecDirName)
	return filepath.Join(dir, ipsecCertFileName), filepath.Join(dir, ipsecKeyFileName)
}

// GenerateIPSecPeerCert issues an IKEv2 peer certificate for this node signed
// by the existing cluster CA. The cert carries the IPSec-IKE EKU so strongSwan
// accepts it. The dnsName SAN must equal the OVS chassis-id (system-id).
func GenerateIPSecPeerCert(configDir, caCertPath, caKeyPath, hostname, nodeIP string) error {
	if hostname == "" {
		return fmt.Errorf("ipsec peer cert: hostname required")
	}
	if nodeIP == "" {
		return fmt.Errorf("ipsec peer cert: nodeIP required")
	}
	parsedIP := net.ParseIP(nodeIP)
	if parsedIP == nil {
		return fmt.Errorf("ipsec peer cert: invalid nodeIP %q", nodeIP)
	}

	caCert, caKey, err := loadCAKeyPair(caCertPath, caKeyPath)
	if err != nil {
		return err
	}

	peerKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("ipsec peer cert: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("ipsec peer cert: serial: %w", err)
	}

	notBefore := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   hostname,
			Organization: []string{"Spinifex Platform"},
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		UnknownExtKeyUsage:    []asn1.ObjectIdentifier{oidExtKeyUsageIPSecIKE},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
		IPAddresses:           []net.IP{parsedIP},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, caCert, &peerKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("ipsec peer cert: create certificate: %w", err)
	}

	dir := filepath.Join(configDir, ipsecDirName)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("ipsec peer cert: mkdir %s: %w", dir, err)
	}

	certPath, keyPath := IPSecCertPaths(configDir)
	if err := writePEMFile(certPath, "CERTIFICATE", der, 0644); err != nil {
		return fmt.Errorf("ipsec peer cert: write cert: %w", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(peerKey)
	if err != nil {
		return fmt.Errorf("ipsec peer cert: marshal key: %w", err)
	}
	if err := writePEMFile(keyPath, "PRIVATE KEY", keyBytes, 0600); err != nil {
		return fmt.Errorf("ipsec peer cert: write key: %w", err)
	}

	// strongSwan 6.0 only loads CAs placed in /etc/ipsec.d/cacerts at startup;
	// per-conn cacert= references are insufficient. Install the cluster CA there.
	if err := installCAIntoCharonTrustStore(caCertPath); err != nil {
		return fmt.Errorf("ipsec peer cert: install CA into charon trust store: %w", err)
	}

	return nil
}

// charonCATrustDir is the strongSwan default cacerts scan dir. Overridden
// in tests so they don't pollute the real /etc/ipsec.d/cacerts.
var charonCATrustDir = "/etc/ipsec.d/cacerts"

const charonCATrustLink = "spinifex-ca.pem"

func installCAIntoCharonTrustStore(caCertPath string) error {
	if err := os.MkdirAll(charonCATrustDir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", charonCATrustDir, err)
	}
	link := filepath.Join(charonCATrustDir, charonCATrustLink)
	tmp, err := os.MkdirTemp(charonCATrustDir, ".cainstall-")
	if err != nil {
		return fmt.Errorf("tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)
	staging := filepath.Join(tmp, charonCATrustLink)
	if err := os.Symlink(caCertPath, staging); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", staging, caCertPath, err)
	}
	if err := os.Rename(staging, link); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", staging, link, err)
	}

	// Trigger rereadcacerts so charon picks up the symlink without restart.
	// Silent failure is expected when charon isn't running yet.
	if err := charonRereadCAs(); err != nil {
		slog.Debug("ipsec: rereadcacerts skipped", "err", err)
	}
	return nil
}

// charonRereadCAs triggers strongSwan to re-scan /etc/ipsec.d/cacerts so a
// freshly-symlinked CA is picked up by an already-running charon. Overridden
// in tests to avoid execing ipsec.
var charonRereadCAs = func() error {
	out, err := exec.Command("/usr/sbin/ipsec", "rereadcacerts").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ipsec rereadcacerts: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func loadCAKeyPair(caCertPath, caKeyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert %s: %w", caCertPath, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode CA cert PEM at %s", caCertPath)
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key %s: %w", caKeyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decode CA key PEM at %s", caKeyPath)
	}
	caKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	rsaKey, ok := caKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not RSA")
	}
	return caCert, rsaKey, nil
}

func writePEMFile(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
