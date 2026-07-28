package handlers_acm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lowers the RSA key sizes for the whole privateca test file. 2048
// (CA) and 1024 (leaf) bits are still valid RSA keys and generate several
// times faster than the production defaults, and the tests here care about
// certificate structure, not cryptographic strength — mirrors
// admin.certKeyBits's test seam.
func TestMain(m *testing.M) {
	caKeyBits = 2048
	leafKeyBits = 1024
	os.Exit(m.Run())
}

func tenantCAPaths(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "tenant-ca.pem"), filepath.Join(dir, "tenant-ca-key.pem")
}

func TestLoadOrCreateTenantCA_CreatesConstrainedRoot(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)
	require.NotNil(t, ca)

	root := ca.root
	assert.True(t, root.IsCA)
	assert.True(t, root.BasicConstraintsValid)
	assert.Equal(t, 0, root.MaxPathLen)
	assert.True(t, root.MaxPathLenZero)
	assert.Equal(t, x509.KeyUsageCertSign|x509.KeyUsageCRLSign, root.KeyUsage)
	assert.Equal(t, []string{"home.example.com"}, root.PermittedDNSDomains)
	assert.True(t, root.PermittedDNSDomainsCritical)

	// 0.0.0.0/0 and ::/0 must both be present in ExcludedIPRanges so this
	// root can never validate an IP SAN.
	var haveV4, haveV6 bool
	for _, r := range root.ExcludedIPRanges {
		ones, bits := r.Mask.Size()
		if ones == 0 && bits == 32 {
			haveV4 = true
		}
		if ones == 0 && bits == 128 {
			haveV6 = true
		}
	}
	assert.True(t, haveV4, "expected 0.0.0.0/0 in ExcludedIPRanges")
	assert.True(t, haveV6, "expected ::/0 in ExcludedIPRanges")

	assert.Equal(t, []string{""}, root.ExcludedEmailAddresses)
	assert.Equal(t, []string{""}, root.ExcludedURIDomains)

	// Files must exist on disk with the key locked down to 0600.
	certInfo, err := os.Stat(certPath)
	require.NoError(t, err)
	assert.False(t, certInfo.IsDir())

	keyInfo, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), keyInfo.Mode().Perm())
}

func TestLoadOrCreateTenantCA_RefusesEmptyPermittedDomains(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadOrCreateTenantCA(certPath, keyPath, nil)
	require.Error(t, err)
	assert.NoFileExists(t, certPath)
	assert.NoFileExists(t, keyPath)
}

func TestLoadOrCreateTenantCA_Idempotent(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	first, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	keyInfoBefore, err := os.Stat(keyPath)
	require.NoError(t, err)

	second, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	// Same root: identical serial number proves the second call loaded
	// rather than regenerated.
	assert.Equal(t, 0, first.root.SerialNumber.Cmp(second.root.SerialNumber))
	assert.Equal(t, first.rootPEM, second.rootPEM)

	keyInfoAfter, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), keyInfoAfter.Mode().Perm())
	assert.Equal(t, keyInfoBefore.ModTime(), keyInfoAfter.ModTime(), "second call must not rewrite the key file")
}

func TestLoadOrCreateTenantCA_DriftIsRejectedAndLeavesRootUntouched(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	certBefore, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyBefore, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	_, err = LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com", "evil.example.org"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evil.example.org")
	assert.Contains(t, err.Error(), "regenerate")

	certAfter, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyAfter, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, certBefore, certAfter, "drift must not touch the on-disk cert")
	assert.Equal(t, keyBefore, keyAfter, "drift must not touch the on-disk key")
}

func TestLoadOrCreateTenantCA_SubdomainRequestCoveredByExistingApex(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"example.com"})
	require.NoError(t, err)

	// A narrower request (a subdomain of an already-covered apex) must load
	// cleanly — only a domain the root does NOT cover is drift.
	_, err = LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)
}

func TestLoadOrCreateTenantCA_IncompletePairIsRejected(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	require.NoError(t, os.Remove(keyPath))

	_, err = LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
	assert.NoFileExists(t, keyPath, "must not regenerate a key next to a surviving cert")
}

func TestIssueLeaf_ChainsToRootAndCoversWildcard(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	certPEM, chainPEM, keyPEM, err := ca.IssueLeaf("*.home.example.com", []string{"home.example.com"})
	require.NoError(t, err)
	require.NotEmpty(t, certPEM)
	require.NotEmpty(t, chainPEM)
	require.NotEmpty(t, keyPEM)

	leaf := parseCertPEM(t, certPEM)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(chainPEM)))

	opts := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Exact apex SAN.
	opts.DNSName = "home.example.com"
	_, err = leaf.Verify(opts)
	require.NoError(t, err)

	// Wildcard SAN covering an arbitrary subdomain.
	opts.DNSName = "foo.home.example.com"
	_, err = leaf.Verify(opts)
	require.NoError(t, err)
}

func TestIssueLeaf_RejectsDomainOutsidePermittedDomains(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	_, _, _, err = ca.IssueLeaf("evil.example.org", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evil.example.org")
}

// TestLeafOutsidePermittedDomains_FailsVerify is the test that proves the
// name constraints on the tenant root are cryptographically real rather than
// a policy check IssueLeaf happens to apply. It signs a leaf for a
// domain outside PermittedDNSDomains directly against the CA's own key,
// bypassing IssueLeaf's Authorized guard entirely — simulating exactly the
// "our server-side check is bypassed or buggy" scenario the design doc
// calls out. If this leaf were to verify, the constraints baked into the
// root would be decorative, not enforced.
func TestLeafOutsidePermittedDomains_FailsVerify(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	leafPEM := forgeLeaf(t, ca, &x509.Certificate{
		DNSNames: []string{"evil.example.org"},
	})
	leaf := parseCertPEM(t, leafPEM)

	pool := x509.NewCertPool()
	pool.AddCert(ca.root)

	_, err = leaf.Verify(x509.VerifyOptions{
		DNSName:   "evil.example.org",
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.Error(t, err, "a leaf for a domain outside PermittedDNSDomains must fail verification")
	var constraintErr x509.CertificateInvalidError
	if assert.ErrorAs(t, err, &constraintErr) {
		assert.Equal(t, x509.CANotAuthorizedForThisName, constraintErr.Reason)
	}
}

// TestLeafWithIPSAN_FailsVerify proves the excluded 0.0.0.0/0 and ::/0
// ranges actually block IP SANs, again by forging a leaf directly against
// the CA key so the test does not depend on IssueLeaf ever choosing to
// offer an IP SAN option.
func TestLeafWithIPSAN_FailsVerify(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	leafPEM := forgeLeaf(t, ca, &x509.Certificate{
		DNSNames:    []string{"home.example.com"},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.1")},
	})
	leaf := parseCertPEM(t, leafPEM)

	pool := x509.NewCertPool()
	pool.AddCert(ca.root)

	_, err = leaf.Verify(x509.VerifyOptions{
		// VerifyHostname matches an IP-address-shaped DNSName against
		// IPAddresses SANs (RFC 6125 Appendix B.2) — this is the standard way
		// to exercise IP SAN validation through VerifyOptions.
		DNSName:   "10.0.0.1",
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.Error(t, err, "a leaf carrying an IP SAN must fail verification against this root")
}

// TestTenantRootCannotSignSubordinateCA proves MaxPathLen 0 actually forbids
// a subordinate CA: it forges an intermediate CA cert directly beneath the
// tenant root (again bypassing the public API, which offers no way to mint
// one), uses that intermediate to sign a leaf, and asserts the resulting
// chain fails verification even though every individual signature is valid.
func TestTenantRootCannotSignSubordinateCA(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	subKey := generateTestRSAKey(t)
	subTemplate := &x509.Certificate{
		SerialNumber:          testSerial(t),
		Subject:               pkixCN(t, "Tenant Sub CA"),
		NotBefore:             ca.root.NotBefore,
		NotAfter:              ca.root.NotAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	subDER, err := x509.CreateCertificate(rngForTest(), subTemplate, ca.root, &subKey.PublicKey, ca.key)
	require.NoError(t, err)
	subCert, err := x509.ParseCertificate(subDER)
	require.NoError(t, err)

	leafKey := generateTestRSAKey(t)
	leafTemplate := &x509.Certificate{
		SerialNumber:          testSerial(t),
		Subject:               pkixCN(t, "home.example.com"),
		NotBefore:             ca.root.NotBefore,
		NotAfter:              ca.root.NotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"home.example.com"},
	}
	leafDER, err := x509.CreateCertificate(rngForTest(), leafTemplate, subCert, &leafKey.PublicKey, subKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(leafDER)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(ca.root)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(subCert)

	_, err = leaf.Verify(x509.VerifyOptions{
		DNSName:       "home.example.com",
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.Error(t, err, "a leaf issued through a subordinate CA beneath a pathlen-0 root must fail verification")
}

func TestAuthorized(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	cases := []struct {
		name   string
		domain string
		want   bool
	}{
		{"apex", "home.example.com", true},
		{"subdomain", "foo.home.example.com", true},
		{"deep subdomain", "a.b.home.example.com", true},
		{"wildcard", "*.home.example.com", true},
		{"case-insensitive apex", "HOME.EXAMPLE.COM", true},
		{"case-insensitive subdomain", "Foo.Home.Example.Com", true},
		{"unrelated domain", "example.org", false},
		{"superstring, not a subdomain", "nothome.example.com", false},
		{"parent of permitted apex", "example.com", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ca.Authorized(tc.domain))
		})
	}
}

func TestLoadTenantCA_MissingReturnsActionableError(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadTenantCA(certPath, keyPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), tenantCACreateCommandHint, "error must name the admin command that creates the CA")
}

func TestLoadTenantCA_IncompletePairReturnsActionableError(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	_, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)
	require.NoError(t, os.Remove(keyPath))

	_, err = LoadTenantCA(certPath, keyPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
	assert.Contains(t, err.Error(), tenantCACreateCommandHint)
}

func TestLoadTenantCA_RoundTripsCreatedCA(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)

	created, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com", "lab.example.org"})
	require.NoError(t, err)

	loaded, err := LoadTenantCA(certPath, keyPath)
	require.NoError(t, err)

	// Same root: identical serial number and PEM prove LoadTenantCA loaded
	// rather than fabricated anything.
	assert.Equal(t, 0, created.root.SerialNumber.Cmp(loaded.root.SerialNumber))
	assert.Equal(t, created.rootPEM, loaded.rootPEM)
	assert.Equal(t, []string{"home.example.com", "lab.example.org"}, loaded.PermittedDomains())

	// PermittedDomains reflects exactly what was baked in at creation, read
	// back from the certificate rather than a parallel list.
	assert.True(t, loaded.Authorized("home.example.com"))
	assert.True(t, loaded.Authorized("foo.lab.example.org"))
	assert.False(t, loaded.Authorized("evil.example.net"))
}

func TestPermittedDomains_ReturnsDefensiveCopy(t *testing.T) {
	certPath, keyPath := tenantCAPaths(t)
	ca, err := LoadOrCreateTenantCA(certPath, keyPath, []string{"home.example.com"})
	require.NoError(t, err)

	got := ca.PermittedDomains()
	require.Equal(t, []string{"home.example.com"}, got)

	got[0] = "mutated.example.com"
	assert.Equal(t, []string{"home.example.com"}, ca.PermittedDomains(), "mutating the returned slice must not affect the CA")
}

// -- test helpers --------------------------------------------------------

// forgeLeaf signs overrides directly against ca's root and key, filling in
// only the fields a real caller of IssueLeaf would not control, so tests can
// construct leaves IssueLeaf itself would refuse to produce.
func forgeLeaf(t *testing.T, ca *TenantCA, overrides *x509.Certificate) string {
	t.Helper()
	key := generateTestRSAKey(t)
	overrides.SerialNumber = testSerial(t)
	if overrides.Subject.CommonName == "" && len(overrides.DNSNames) > 0 {
		overrides.Subject = pkixCN(t, overrides.DNSNames[0])
	}
	overrides.NotBefore = ca.root.NotBefore
	overrides.NotAfter = ca.root.NotAfter
	overrides.KeyUsage = x509.KeyUsageDigitalSignature
	overrides.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	overrides.BasicConstraintsValid = true

	der, err := x509.CreateCertificate(rngForTest(), overrides, ca.root, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func parseCertPEM(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

// generateTestRSAKey returns a key sized for fast test signing — not
// production-strength, and never used outside this test file.
func generateTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, leafKeyBits)
	require.NoError(t, err)
	return key
}

// testSerial returns a random 128-bit serial number for a forged test
// certificate.
func testSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := randomSerial()
	require.NoError(t, err)
	return serial
}

// pkixCN builds a minimal Subject carrying only a CommonName, for forged
// test certificates that do not go through IssueLeaf.
func pkixCN(t *testing.T, cn string) pkix.Name {
	t.Helper()
	return pkix.Name{CommonName: cn}
}

// rngForTest is crypto/rand.Reader, named for readability at x509.CreateCertificate call sites in this file.
func rngForTest() io.Reader {
	return rand.Reader
}
