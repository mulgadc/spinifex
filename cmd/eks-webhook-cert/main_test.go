package main

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

const (
	testCN  = "aws-load-balancer-webhook-service.kube-system.svc"
	testDNS = "aws-load-balancer-webhook-service.kube-system.svc.cluster.local"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(decodeFirst(certPEM))
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return leaf
}

func TestEnsureCertMintsServingCert(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, err := ensureCert(dir, testCN, []string{testCN, testDNS})
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("empty key PEM")
	}
	leaf := parseLeaf(t, certPEM)

	// Both webhook service DNS names must be present, or the apiserver's TLS
	// dial to the webhook fails SAN verification.
	for _, want := range []string{testCN, testDNS} {
		if err := leaf.VerifyHostname(want); err != nil {
			t.Errorf("SAN %q missing: %v", want, err)
		}
	}
	if !leaf.IsCA {
		t.Error("cert must be a CA so the same PEM serves as the webhook caBundle")
	}

	// The single self-signed cert must verify against itself as a root — this is
	// exactly what the apiserver does with caBundle == the serving cert.
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   testCN,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("self-signed cert does not verify as its own CA: %v", err)
	}
}

func TestEnsureCertReusesCachedPair(t *testing.T) {
	dir := t.TempDir()
	cert1, key1, err := ensureCert(dir, testCN, []string{testCN})
	if err != nil {
		t.Fatalf("first ensureCert: %v", err)
	}
	cert2, key2, err := ensureCert(dir, testCN, []string{testCN})
	if err != nil {
		t.Fatalf("second ensureCert: %v", err)
	}
	if string(cert1) != string(cert2) || string(key1) != string(key2) {
		t.Error("ensureCert regenerated instead of reusing the cached pair; a churning cert re-applies the addon every tick")
	}

	// The private key must not be world-readable on disk.
	info, err := os.Stat(filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("tls.key mode %o is group/other-accessible", info.Mode().Perm())
	}
}
