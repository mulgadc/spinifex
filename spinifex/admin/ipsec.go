package admin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
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

// IPSecCertPaths returns the canonical paths for the per-node IPsec peer
// credential pair under configDir. Both paths live in
// {configDir}/ipsec/. The CA trust anchor is the existing cluster CA in
// configDir/ca.pem; ovs-monitor-ipsec is pointed at that path directly.
func IPSecCertPaths(configDir string) (certPath, keyPath string) {
	dir := filepath.Join(configDir, ipsecDirName)
	return filepath.Join(dir, ipsecCertFileName), filepath.Join(dir, ipsecKeyFileName)
}

// GenerateIPSecPeerCert issues an IKEv2 peer certificate for this node,
// signed by the existing cluster CA. The cert carries the IPSec-IKE
// extended key usage so strongSwan accepts it as a peer credential.
//
// hostname is the node's primary hostname (used as Common Name + DNS SAN).
// nodeIP is the node's primary cluster IP (added as an IP SAN). Additional
// SANs discovered from the local machine's interfaces are not added here —
// IPSec peer identity should be the explicit cluster identity only, not
// every interface address, to keep peer-identity matching strict.
// GenerateIPSecPeerCertIfEnabled is a no-op when enabled is false. Used by
// admin init and admin join so the call site reads as a single guarded line
// keyed off the cluster's network.ipsec_enabled flag.
func GenerateIPSecPeerCertIfEnabled(enabled bool, configDir, hostname, nodeIP string) error {
	if !enabled {
		return nil
	}
	caCertPath := filepath.Join(configDir, "ca.pem")
	caKeyPath := filepath.Join(configDir, "ca.key")
	return GenerateIPSecPeerCert(configDir, caCertPath, caKeyPath, hostname, nodeIP)
}

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
