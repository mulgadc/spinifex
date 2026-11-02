package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinnedCert(t *testing.T, uris ...string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range uris {
		u, err := url.Parse(s)
		require.NoError(t, err)
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

func TestVerifyPinned(t *testing.T) {
	const want = "spinifex-lambda://r/a/f/e/g/guest"
	pin := pinnedCert(t, want)
	other := pinnedCert(t, want)
	twoURIs := pinnedCert(t, want, "spinifex-lambda://r/a/f/e/g/host")

	require.NoError(t, VerifyPinned([]*x509.Certificate{pin}, pin, x509.ExtKeyUsageClientAuth, want))

	cases := []struct {
		name  string
		chain []*x509.Certificate
		pin   *x509.Certificate
		usage x509.ExtKeyUsage
		uri   string
		msg   string
	}{
		{"no pin", []*x509.Certificate{pin}, nil, x509.ExtKeyUsageClientAuth, want, "no pinned peer certificate"},
		{"empty chain", nil, pin, x509.ExtKeyUsageClientAuth, want, "sent 0 certificates"},
		{"extra chain certificate", []*x509.Certificate{pin, other}, pin, x509.ExtKeyUsageClientAuth, want, "sent 2 certificates"},
		{"same identity, other key", []*x509.Certificate{other}, pin, x509.ExtKeyUsageClientAuth, want, "not the pinned certificate"},
		{"wrong usage", []*x509.Certificate{pin}, pin, x509.ExtKeyUsageServerAuth, want, "incompatible key usage"},
		{"wrong URI", []*x509.Certificate{pin}, pin, x509.ExtKeyUsageClientAuth, "spinifex-lambda://r/a/f/e/g2/guest", "identity mismatch"},
		{"second URI", []*x509.Certificate{twoURIs}, twoURIs, x509.ExtKeyUsageClientAuth, want, "identity mismatch"},
	}
	for _, tc := range cases {
		err := VerifyPinned(tc.chain, tc.pin, tc.usage, tc.uri)
		assert.ErrorContains(t, err, tc.msg, tc.name)
	}
	assert.ErrorIs(t, VerifyPinned([]*x509.Certificate{pin}, pin, x509.ExtKeyUsageClientAuth, "x://y"), ErrPeerIdentity)
}
