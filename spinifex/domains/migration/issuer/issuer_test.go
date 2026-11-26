package issuer

import (
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func newTestCA(t *testing.T, notBefore, notAfter time.Time, isCA bool) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func newTestIssuer(t *testing.T) (*Issuer, *x509.Certificate) {
	t.Helper()
	ca, key := newTestCA(t, testNow.Add(-24*time.Hour), testNow.Add(24*time.Hour), true)
	iss, err := New(ca, key, "c1", func() time.Time { return testNow })
	require.NoError(t, err)
	return iss, ca
}

func TestIssue_CertificateShape(t *testing.T) {
	iss, ca := newTestIssuer(t)
	cc, err := iss.Issue("123456789012", "conn-1")
	require.NoError(t, err)
	c := cc.Certificate

	assert.Equal(t, "spx-conn-1", c.Subject.CommonName)
	assert.Equal(t, []string{"123456789012"}, c.Subject.OrganizationalUnit)
	assert.Equal(t, []string{"spinifex-migration-c1"}, c.Subject.Organization)
	assert.False(t, c.IsCA)
	assert.True(t, c.BasicConstraintsValid)
	assert.Equal(t, x509.KeyUsageDigitalSignature, c.KeyUsage)
	assert.Empty(t, c.ExtKeyUsage)
	assert.Equal(t, x509.SHA256WithRSA, c.SignatureAlgorithm)
	assert.Equal(t, elliptic.P256(), cc.Key.Curve)
	assert.Equal(t, testNow.Add(CertLifetime), c.NotAfter)
	assert.True(t, c.NotBefore.Before(testNow))
	assert.Equal(t, 1, c.SerialNumber.Sign())

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = c.Verify(x509.VerifyOptions{
		Roots: pool, CurrentTime: testNow, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	assert.NoError(t, err)
	assert.True(t, IsMigrationSubject(c.Subject))
}

func TestIssue_RefusesBadSubjectParts(t *testing.T) {
	iss, _ := newTestIssuer(t)
	_, err := iss.Issue("123456789012", "a,OU=other")
	assert.Error(t, err)
	_, err = iss.Issue("not-an-account", "conn-1")
	assert.Error(t, err)
}

func TestIssue_ClusterIDComesFromIssuer(t *testing.T) {
	ca, key := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(time.Hour), true)
	a, err := New(ca, key, "cluster-a", func() time.Time { return testNow })
	require.NoError(t, err)
	b, err := New(ca, key, "cluster-b", func() time.Time { return testNow })
	require.NoError(t, err)
	ca1, err := a.Issue("123456789012", "c")
	require.NoError(t, err)
	cb, err := b.Issue("123456789012", "c")
	require.NoError(t, err)
	assert.NotEqual(t, ca1.Certificate.Subject.Organization, cb.Certificate.Subject.Organization)
}

func TestIssue_NotAfterCappedByCA(t *testing.T) {
	ca, key := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(20*time.Minute), true)
	iss, err := New(ca, key, "c1", func() time.Time { return testNow })
	require.NoError(t, err)
	cc, err := iss.Issue("123456789012", "c")
	require.NoError(t, err)
	assert.Equal(t, ca.NotAfter, cc.Certificate.NotAfter)
}

func TestIssue_RefusesExpiredCA(t *testing.T) {
	ca, key := newTestCA(t, testNow.Add(-48*time.Hour), testNow.Add(-time.Hour), true)
	iss, err := New(ca, key, "c1", func() time.Time { return testNow })
	require.NoError(t, err)
	_, err = iss.Issue("123456789012", "c")
	assert.Error(t, err)
}

func TestNew_Rejects(t *testing.T) {
	ca, key := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(time.Hour), true)
	_, otherKey := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(time.Hour), true)
	leaf, leafKey := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(time.Hour), false)
	now := func() time.Time { return testNow }

	_, err := New(nil, key, "c1", now)
	assert.Error(t, err)
	_, err = New(ca, otherKey, "c1", now)
	assert.Error(t, err, "key must match the certificate")
	_, err = New(leaf, leafKey, "c1", now)
	assert.Error(t, err, "a non-CA certificate cannot issue")
	_, err = New(ca, key, "bad id", now)
	assert.Error(t, err)
}

func TestLoad_ReadsPlatformCAFiles(t *testing.T) {
	ca, key := newTestCA(t, testNow.Add(-time.Hour), testNow.Add(time.Hour), true)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600))
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))

	iss, err := Load(certPath, keyPath, "c1", func() time.Time { return testNow })
	require.NoError(t, err)
	_, err = iss.Issue("123456789012", "c")
	assert.NoError(t, err)

	_, err = Load(filepath.Join(dir, "missing.pem"), keyPath, "c1", nil)
	assert.Error(t, err)
}

func TestClientCertificate_RedactsKey(t *testing.T) {
	iss, _ := newTestIssuer(t)
	cc, err := iss.Issue("123456789012", "c")
	require.NoError(t, err)
	assert.NotContains(t, cc.String(), cc.Key.D.String())
	assert.NotContains(t, cc.GoString(), cc.Key.D.String())
}
