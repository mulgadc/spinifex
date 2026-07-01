package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCA mints a self-signed EC CA (cert + SEC1 key PEM) into dir, returning
// the two paths — a stand-in for K3s' /var/lib/rancher/k3s/server/tls/server-ca.*.
func writeTestCA(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "k3s-server-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ca key: %v", err)
	}
	certPath = filepath.Join(dir, "server-ca.crt")
	keyPath = filepath.Join(dir, "server-ca.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write ca cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write ca key: %v", err)
	}
	return certPath, keyPath
}

func leafFrom(t *testing.T, certPath string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	crt, err := x509.ParseCertificate(decodeFirst(b))
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return crt
}

func TestEnsureCertSignsLeafWithSANsThatChainsToCA(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := writeTestCA(t, dir)

	dns, ips := splitSANs("10.32.100.4,203.0.113.9,eks-toc.example")
	certPath, keyPath, err := ensureCert(dir, caCert, caKey, "konnectivity-server", dns, ips)
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key not written: %v", err)
	}

	leaf := leafFrom(t, certPath)
	if leaf.IsCA {
		t.Error("konnectivity serving cert must be a leaf, not a CA")
	}
	// The agent dials the NLB private-endpoint IP and verifies via the in-pod CA,
	// so the IP must be a SAN or the tunnel TLS handshake fails.
	if err := leaf.VerifyHostname("10.32.100.4"); err != nil {
		t.Errorf("IP SAN 10.32.100.4 missing: %v", err)
	}

	// The leaf must chain to the K3s server CA — that CA is the in-pod ca.crt the
	// agent trusts, so no separate CA distribution is needed.
	caPEM, _ := os.ReadFile(caCert)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA failed")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   "eks-toc.example",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not chain to the K3s server CA: %v", err)
	}
}

func TestEnsureCertReusesThenReissuesOnSANChange(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := writeTestCA(t, dir)

	dns, ips := splitSANs("10.32.100.4,eks-toc.example")
	c1, _, err := ensureCert(dir, caCert, caKey, "konnectivity-server", dns, ips)
	if err != nil {
		t.Fatalf("first ensureCert: %v", err)
	}
	first, _ := os.ReadFile(c1)

	// Same SANs → cached pair reused byte-for-byte (a churning cert would bounce
	// the konnectivity-server on every restart).
	if _, _, err := ensureCert(dir, caCert, caKey, "konnectivity-server", dns, ips); err != nil {
		t.Fatalf("second ensureCert: %v", err)
	}
	if second, _ := os.ReadFile(c1); string(first) != string(second) {
		t.Error("ensureCert re-minted instead of reusing the SAN-current cached cert")
	}

	// A new endpoint in the SAN set must force a re-issue.
	dns2, ips2 := splitSANs("10.32.100.4,eks-toc.example,10.32.100.99")
	if _, _, err := ensureCert(dir, caCert, caKey, "konnectivity-server", dns2, ips2); err != nil {
		t.Fatalf("third ensureCert: %v", err)
	}
	if third, _ := os.ReadFile(c1); string(first) == string(third) {
		t.Error("ensureCert served a stale cert that does not cover the new SAN")
	}
	if err := leafFrom(t, c1).VerifyHostname("10.32.100.99"); err != nil {
		t.Errorf("re-issued cert missing new SAN: %v", err)
	}
}

func TestSplitSANsPartitionsIPsAndDNS(t *testing.T) {
	dns, ips := splitSANs(" 10.0.0.1 , host.example , , 2001:db8::1 ")
	if len(dns) != 1 || dns[0] != "host.example" {
		t.Errorf("dns = %v, want [host.example]", dns)
	}
	wantIPs := []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("2001:db8::1")}
	if len(ips) != len(wantIPs) {
		t.Fatalf("ips = %v, want %v", ips, wantIPs)
	}
	for i, ip := range ips {
		if !ip.Equal(wantIPs[i]) {
			t.Errorf("ips[%d] = %v, want %v", i, ip, wantIPs[i])
		}
	}
}
