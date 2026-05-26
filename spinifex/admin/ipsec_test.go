package admin

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateIPSecPeerCert shares a CA across subtests so the 4096-bit
// CA-key generation runs once (~0.7s).
func TestGenerateIPSecPeerCert(t *testing.T) {
	// Redirect the charon trust-store symlink at a temp dir so the test
	// doesn't try to write to /etc/ipsec.d/cacerts on the host. Restored
	// in cleanup. Cannot use t.Parallel() with this var override.
	origTrustDir := charonCATrustDir
	charonCATrustDir = t.TempDir()
	t.Cleanup(func() { charonCATrustDir = origTrustDir })

	// Stub the ipsec rereadcacerts shell-out so tests don't depend on
	// strongSwan being installed/running on the test host.
	origReread := charonRereadCAs
	charonRereadCAs = func() error { return nil }
	t.Cleanup(func() { charonRereadCAs = origReread })

	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "node-1", "10.0.0.5"))

		certPath, keyPath := IPSecCertPaths(dir)
		certPEM, err := os.ReadFile(certPath)
		require.NoError(t, err)
		block, _ := pem.Decode(certPEM)
		require.NotNil(t, block)
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)

		assert.Equal(t, "node-1", cert.Subject.CommonName)
		assert.Equal(t, []string{"node-1"}, cert.DNSNames)
		require.Len(t, cert.IPAddresses, 1)
		assert.True(t, cert.IPAddresses[0].Equal(net.ParseIP("10.0.0.5")))

		// IPSec EKU must be present so strongSwan accepts the cert as an IKEv2 peer.
		var hasIPSecIKE bool
		for _, oid := range cert.UnknownExtKeyUsage {
			if oid.Equal(oidExtKeyUsageIPSecIKE) {
				hasIPSecIKE = true
				break
			}
		}
		assert.True(t, hasIPSecIKE, "cert missing id-kp-ipsecIKE EKU")

		caCertPEM, _ := os.ReadFile(caCertPath)
		caBlock, _ := pem.Decode(caCertPEM)
		caCert, _ := x509.ParseCertificate(caBlock.Bytes)
		pool := x509.NewCertPool()
		pool.AddCert(caCert)
		_, err = cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		assert.NoError(t, err, "cert must verify against cluster CA")

		info, err := os.Stat(keyPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "key must be 0600")
	})

	t.Run("idempotent overwrite", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "node-2", "10.0.0.6"))
		require.NoError(t, GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "node-2", "10.0.0.7"))

		certPath, _ := IPSecCertPaths(dir)
		certPEM, _ := os.ReadFile(certPath)
		block, _ := pem.Decode(certPEM)
		cert, _ := x509.ParseCertificate(block.Bytes)
		assert.True(t, cert.IPAddresses[0].Equal(net.ParseIP("10.0.0.7")), "second call should rotate the SAN IP")
	})

	t.Run("rejects missing hostname", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "", "10.0.0.5")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hostname required")
	})

	t.Run("rejects missing nodeIP", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "node-1", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nodeIP required")
	})

	t.Run("rejects invalid nodeIP", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := GenerateIPSecPeerCert(dir, caCertPath, caKeyPath, "node-1", "not-an-ip")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid nodeIP")
	})

	t.Run("missing CA file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		err := GenerateIPSecPeerCert(dir, filepath.Join(dir, "ghost-ca.pem"), caKeyPath, "node-1", "10.0.0.5")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read CA cert")
	})
}

func TestGenerateIPSecPeerCertIfEnabled(t *testing.T) {
	origTrustDir := charonCATrustDir
	charonCATrustDir = t.TempDir()
	t.Cleanup(func() { charonCATrustDir = origTrustDir })

	origReread := charonRereadCAs
	charonRereadCAs = func() error { return nil }
	t.Cleanup(func() { charonRereadCAs = origReread })

	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca.key")
	require.NoError(t, GenerateCACert(caCertPath, caKeyPath))

	t.Run("disabled is a no-op", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, GenerateIPSecPeerCertIfEnabled(false, dir, "node-x", "10.0.0.9"))
		certPath, _ := IPSecCertPaths(dir)
		_, err := os.Stat(certPath)
		assert.True(t, os.IsNotExist(err), "no cert must be written when ipsec disabled")
	})

	t.Run("enabled generates cert using configDir CA", func(t *testing.T) {
		t.Parallel()
		// Use a configDir that already has ca.pem + ca.key beside it (the
		// canonical layout under /etc/spinifex/).
		dir := t.TempDir()
		require.NoError(t, GenerateCACert(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")))

		require.NoError(t, GenerateIPSecPeerCertIfEnabled(true, dir, "node-y", "10.0.0.10"))
		certPath, keyPath := IPSecCertPaths(dir)
		_, err := os.Stat(certPath)
		require.NoError(t, err)
		_, err = os.Stat(keyPath)
		require.NoError(t, err)
	})
}

func TestIPSecCertPaths(t *testing.T) {
	t.Parallel()
	certPath, keyPath := IPSecCertPaths("/etc/spinifex")
	assert.Equal(t, "/etc/spinifex/ipsec/peer.pem", certPath)
	assert.Equal(t, "/etc/spinifex/ipsec/peer.key", keyPath)
}

// TestInstallCAIntoCharonTrustStore_TriggersRereadCAs locks the fix for the
// race where openvswitch-ipsec started before admin init's CA install: every
// IKE_AUTH failed with `no trusted RSA public key found for '<peer>'` because
// charon had scanned cacerts before the symlink was placed. The trust-store
// installer must trigger `ipsec rereadcacerts` so an already-running charon
// picks up the CA without a restart.
func TestInstallCAIntoCharonTrustStore_TriggersRereadCAs(t *testing.T) {
	origTrustDir := charonCATrustDir
	charonCATrustDir = t.TempDir()
	t.Cleanup(func() { charonCATrustDir = origTrustDir })

	origReread := charonRereadCAs
	var called int
	charonRereadCAs = func() error {
		called++
		return nil
	}
	t.Cleanup(func() { charonRereadCAs = origReread })

	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	require.NoError(t, os.WriteFile(caCertPath, []byte("dummy"), 0644))

	require.NoError(t, installCAIntoCharonTrustStore(caCertPath))
	assert.Equal(t, 1, called, "rereadcacerts must be invoked exactly once after symlink placement")

	link := filepath.Join(charonCATrustDir, charonCATrustLink)
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, caCertPath, target, "symlink target must match CA cert path")
}

// TestInstallCAIntoCharonTrustStore_SwallowsRereadError verifies that a failed
// rereadcacerts (e.g. charon not running, ipsec binary missing on a fresh
// install) does not break admin init. The CA symlink must still be in place so
// a later charon start picks it up via the normal cacerts scan.
func TestInstallCAIntoCharonTrustStore_SwallowsRereadError(t *testing.T) {
	origTrustDir := charonCATrustDir
	charonCATrustDir = t.TempDir()
	t.Cleanup(func() { charonCATrustDir = origTrustDir })

	origReread := charonRereadCAs
	charonRereadCAs = func() error { return stubError("charon not running") }
	t.Cleanup(func() { charonRereadCAs = origReread })

	caDir := t.TempDir()
	caCertPath := filepath.Join(caDir, "ca.pem")
	require.NoError(t, os.WriteFile(caCertPath, []byte("dummy"), 0644))

	require.NoError(t, installCAIntoCharonTrustStore(caCertPath),
		"reread failure must not propagate; admin init has to succeed even when charon is offline")

	link := filepath.Join(charonCATrustDir, charonCATrustLink)
	_, err := os.Readlink(link)
	require.NoError(t, err, "symlink must persist even when reread fails")
}

type stubError string

func (e stubError) Error() string { return string(e) }
