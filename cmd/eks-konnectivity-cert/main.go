// Command eks-konnectivity-cert mints, then reuses, an ECDSA-P256 serving
// certificate for the on-node konnectivity-server, signed by the K3s cluster CA.
//
// The konnectivity-agent (DaemonSet on workers) verifies the server it dials
// using the in-pod ServiceAccount CA (/var/run/secrets/.../ca.crt), which in
// K3s is the same server-ca that signs the apiserver. Signing the konnectivity
// serving cert with that CA is therefore what lets the agent validate the
// reverse-tunnel endpoint with no separate CA distribution.
//
// The eks-node AMI carries no openssl, so this is the Go equivalent: load the
// K3s server CA cert+key, issue a leaf (ServerAuth) cert carrying the cluster's
// konnectivity SANs (the NLB private-endpoint IP, public endpoint IP, NLB DNS),
// and cache it under -dir so the OpenRC service is byte-stable across restarts.
//
// Usage:
//
//	eks-konnectivity-cert -dir /var/lib/spinifex-eks/konnectivity \
//	    -ca-cert /var/lib/rancher/k3s/server/tls/server-ca.crt \
//	    -ca-key  /var/lib/rancher/k3s/server/tls/server-ca.key \
//	    -cn konnectivity-server -sans 10.32.100.4,203.0.113.9,eks-toc.example
//
// Writes <dir>/tls.crt and <dir>/tls.key; prints the two paths (tab-separated).
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	dir := flag.String("dir", "", "directory to cache tls.crt/tls.key in")
	caCert := flag.String("ca-cert", "", "PEM path of the signing (K3s server) CA cert")
	caKey := flag.String("ca-key", "", "PEM path of the signing CA private key")
	cn := flag.String("cn", "konnectivity-server", "certificate common name")
	sansCSV := flag.String("sans", "", "comma-separated SANs (IP or DNS)")
	flag.Parse()

	if *dir == "" || *caCert == "" || *caKey == "" || *sansCSV == "" {
		fatal("eks-konnectivity-cert: -dir, -ca-cert, -ca-key and -sans are required")
	}
	dns, ips := splitSANs(*sansCSV)
	if len(dns) == 0 && len(ips) == 0 {
		fatal("eks-konnectivity-cert: -sans lists no usable names")
	}

	certPath, keyPath, err := ensureCert(*dir, *caCert, *caKey, *cn, dns, ips)
	if err != nil {
		fatal("eks-konnectivity-cert: " + err.Error())
	}
	fmt.Printf("%s\t%s\n", certPath, keyPath)
}

// ensureCert returns the cached cert/key paths under dir, minting and persisting
// a fresh CA-signed pair when none is present, readable, and still SAN-current.
func ensureCert(dir, caCertPath, caKeyPath, cn string, dns []string, ips []net.IP) (string, string, error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if certCoversSANs(certPath, keyPath, dns, ips) {
		return certPath, keyPath, nil
	}

	caCert, caKey, err := loadCA(caCertPath, caKeyPath)
	if err != nil {
		return "", "", err
	}
	certPEM, keyPEM, err := issueLeaf(caCert, caKey, cn, dns, ips)
	if err != nil {
		return "", "", err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err = os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", certPath, err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", fmt.Errorf("write %s: %w", keyPath, err)
	}
	return certPath, keyPath, nil
}

// certCoversSANs reports whether the cached cert exists, parses, has a key, and
// already carries every requested SAN — so a cert minted before the endpoint
// set changed is re-issued rather than served stale.
func certCoversSANs(certPath, keyPath string, dns []string, ips []net.IP) bool {
	c, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	if k, kerr := os.ReadFile(keyPath); kerr != nil || len(k) == 0 {
		return false
	}
	der := decodeFirst(c)
	if der == nil {
		return false
	}
	crt, err := x509.ParseCertificate(der)
	if err != nil {
		return false
	}
	have := make(map[string]struct{}, len(crt.DNSNames)+len(crt.IPAddresses))
	for _, n := range crt.DNSNames {
		have[n] = struct{}{}
	}
	for _, ip := range crt.IPAddresses {
		have[ip.String()] = struct{}{}
	}
	for _, n := range dns {
		if _, ok := have[n]; !ok {
			return false
		}
	}
	for _, ip := range ips {
		if _, ok := have[ip.String()]; !ok {
			return false
		}
	}
	return true
}

// loadCA parses the PEM CA cert + EC/PKCS8 private key used to sign the leaf.
func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read ca cert %s: %w", certPath, err)
	}
	der := decodeFirst(certPEM)
	if der == nil {
		return nil, nil, fmt.Errorf("ca cert %s: no PEM block", certPath)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read ca key %s: %w", keyPath, err)
	}
	keyDER := decodeFirst(keyPEM)
	if keyDER == nil {
		return nil, nil, fmt.Errorf("ca key %s: no PEM block", keyPath)
	}
	caKey, err := parseECKey(keyDER)
	if err != nil {
		return nil, nil, err
	}
	return caCert, caKey, nil
}

// parseECKey accepts the EC SEC1 or PKCS8 encodings K3s may write for the CA key.
func parseECKey(der []byte) (*ecdsa.PrivateKey, error) {
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse ca key (sec1 and pkcs8 failed): %w", err)
	}
	ec, ok := k8.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ca key is %T, want ecdsa", k8)
	}
	return ec, nil
}

// issueLeaf mints an ECDSA-P256 ServerAuth leaf for cn + SANs, signed by caCert/caKey.
func issueLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, dns []string, ips []net.IP) (certPEM, keyPEM []byte, err error) {
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
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, &priv.PublicKey, caKey)
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

// splitSANs partitions the CSV into DNS names and IPs (an entry that parses as an
// IP goes to IPAddresses, else DNSNames), matching how SAN matching is typed.
func splitSANs(s string) (dns []string, ips []net.IP) {
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if ip := net.ParseIP(p); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dns = append(dns, p)
	}
	return dns, ips
}

// decodeFirst returns the DER bytes of the first PEM block, or nil.
func decodeFirst(pemBytes []byte) []byte {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil
	}
	return blk.Bytes
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}
