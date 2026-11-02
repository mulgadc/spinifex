package hostlink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gen1 Scope = "ap-southeast-2/000000000001/fn-1/env-1/gen-1"
	gen2 Scope = "ap-southeast-2/000000000001/fn-1/env-1/gen-2"
)

type testIdent struct {
	cert    tls.Certificate
	leaf    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
}

func newTestIdent(t *testing.T, scope Scope, role string) testIdent {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	u, err := url.Parse(scope.URI(role))
	require.NoError(t, err)
	usage := x509.ExtKeyUsageServerAuth
	if role == RoleGuest {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: role},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true, URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testIdent{cert: c, leaf: leaf, certPEM: certPEM, keyPEM: keyPEM}
}

// hostConfig is the host end as the executor will run it: it pins one guest
// certificate and checks its URI SAN with the same VerifyPeer.
func hostConfig(host testIdent, pinGuest *x509.Certificate, scope Scope) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{host.cert},
		ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{ALPNv1},
		VerifyConnection: func(cs tls.ConnectionState) error {
			return VerifyPeer(cs, pinGuest, x509.ExtKeyUsageClientAuth, scope.URI(RoleGuest))
		},
	}
}

// serve accepts one connection with cfg, writes "ok" if the handshake
// succeeds, and returns the address and the host's handshake result.
func serve(t *testing.T, cfg *tls.Config) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	done := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		tc := tls.Server(raw, cfg)
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		err = tc.Handshake()
		if err == nil {
			_, err = tc.Write([]byte("ok"))
		}
		done <- err
	}()
	return ln.Addr().String(), done
}

func guestIdentity(guest, pinHost testIdent, scope Scope, addr string) Identity {
	return Identity{Cert: guest.cert, HostCert: pinHost.leaf, Scope: scope, HostAddr: addr}
}

func dialAndRead(t *testing.T, id Identity) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, id)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	n, err := c.Read(buf)
	return string(buf[:n]), err
}

func TestDialMutualAuthentication(t *testing.T) {
	host, guest := newTestIdent(t, gen1, RoleHost), newTestIdent(t, gen1, RoleGuest)
	addr, hostErr := serve(t, hostConfig(host, guest.leaf, gen1))

	got, err := dialAndRead(t, guestIdentity(guest, host, gen1, addr))
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	require.NoError(t, <-hostErr)
}

func TestDialRefusesHost(t *testing.T) {
	guest := newTestIdent(t, gen1, RoleGuest)
	pinned := newTestIdent(t, gen1, RoleHost)
	cases := []struct {
		name    string
		host    testIdent
		pin     testIdent
		mutate  func(*tls.Config)
		wantErr string
	}{
		{"unpinned host certificate", newTestIdent(t, gen1, RoleHost), pinned, nil, "peer certificate"},
		{"pinned certificate of a later generation", newTestIdent(t, gen2, RoleHost), testIdent{}, nil, "peer identity mismatch"},
		{"guest role presented as host", newTestIdent(t, gen1, RoleGuest), testIdent{}, nil, "peer certificate"},
		{"no application protocol", pinned, pinned, func(c *tls.Config) { c.NextProtos = nil }, "application protocol"},
		{"TLS 1.2 only", pinned, pinned, func(c *tls.Config) { c.MinVersion, c.MaxVersion = tls.VersionTLS12, tls.VersionTLS12 }, "protocol version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pin := tc.pin
			if pin.leaf == nil {
				pin = tc.host // the guest pins exactly what is presented
			}
			cfg := hostConfig(tc.host, guest.leaf, gen1)
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			addr, _ := serve(t, cfg)
			_, err := dialAndRead(t, guestIdentity(guest, pin, gen1, addr))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// newCASignedHost returns a CA and a host leaf it signed carrying the same URI
// SAN as a legitimate host certificate.
func newCASignedHost(t *testing.T, scope Scope) (ca testIdent, leaf tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	u, err := url.Parse(scope.URI(RoleHost))
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, URIs: []*url.URL{u},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "host"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	require.NoError(t, err)
	return testIdent{leaf: caCert}, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Trusting the pin as a root is not enough: a different leaf it signed, with
// the right URI SAN, must still be refused.
func TestDialRefusesLeafSignedByPin(t *testing.T) {
	guest := newTestIdent(t, gen1, RoleGuest)
	ca, leaf := newCASignedHost(t, gen1)
	for name, chain := range map[string][][]byte{
		"leaf only":    leaf.Certificate,
		"leaf with CA": {leaf.Certificate[0], ca.leaf.Raw},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := hostConfig(testIdent{}, guest.leaf, gen1)
			cfg.Certificates = []tls.Certificate{{Certificate: chain, PrivateKey: leaf.PrivateKey}}
			addr, _ := serve(t, cfg)
			_, err := dialAndRead(t, guestIdentity(guest, ca, gen1, addr))
			require.Error(t, err)
			assert.Regexp(t, "not the pinned certificate|want exactly the pinned one", err.Error())
		})
	}
}

func TestVerifyPeerFailsClosedWithoutPin(t *testing.T) {
	host := newTestIdent(t, gen1, RoleHost)
	cs := tls.ConnectionState{Version: tls.VersionTLS13, NegotiatedProtocol: ALPNv1, PeerCertificates: []*x509.Certificate{host.leaf}}
	assert.ErrorContains(t, VerifyPeer(cs, nil, x509.ExtKeyUsageServerAuth, gen1.URI(RoleHost)), "no pinned peer certificate")
	require.NoError(t, VerifyPeer(cs, host.leaf, x509.ExtKeyUsageServerAuth, gen1.URI(RoleHost)))
}

// Under TLS 1.3 the guest's handshake completes before the host checks the
// guest certificate, so the host's refusal surfaces on the first read.
func TestHostRefusalSurfacesOnFirstRead(t *testing.T) {
	host, guest := newTestIdent(t, gen1, RoleHost), newTestIdent(t, gen1, RoleGuest)
	other := newTestIdent(t, gen1, RoleGuest)
	addr, hostErr := serve(t, hostConfig(host, other.leaf, gen1))

	_, err := dialAndRead(t, guestIdentity(guest, host, gen1, addr))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad certificate")
	assert.ErrorContains(t, <-hostErr, "peer certificate")
}

func TestHostRefusesGuestOfAnotherGeneration(t *testing.T) {
	host, guest := newTestIdent(t, gen1, RoleHost), newTestIdent(t, gen2, RoleGuest)
	addr, hostErr := serve(t, hostConfig(host, guest.leaf, gen1))

	_, err := dialAndRead(t, guestIdentity(guest, host, gen1, addr))
	require.Error(t, err)
	assert.ErrorIs(t, <-hostErr, ErrPeerIdentity)
}

func writeEntries(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range entries {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name, "raw"), b, 0o600))
	}
	return dir
}

func validEntries(t *testing.T) map[string][]byte {
	host, guest := newTestIdent(t, gen1, RoleHost), newTestIdent(t, gen1, RoleGuest)
	return map[string][]byte{
		EntryGuestKey: guest.keyPEM, EntryGuestCert: guest.certPEM, EntryHostCert: host.certPEM,
		EntryScope: []byte(string(gen1) + "\n"), EntryHostAddr: []byte("10.15.8.1:18443\n"),
	}
}

func TestLoadIdentity(t *testing.T) {
	id, err := LoadIdentity(writeEntries(t, validEntries(t)))
	require.NoError(t, err)
	assert.Equal(t, gen1, id.Scope)
	assert.Equal(t, "10.15.8.1:18443", id.HostAddr)
	require.Len(t, id.HostCert.URIs, 1)
	assert.Equal(t, gen1.URI(RoleHost), id.HostCert.URIs[0].String())
}

func TestLoadIdentityRejects(t *testing.T) {
	other := newTestIdent(t, gen1, RoleGuest)
	cases := []struct {
		name    string
		edit    func(map[string][]byte)
		wantErr string
	}{
		{"missing entry", func(e map[string][]byte) { delete(e, EntryHostCert) }, "read host-cert"},
		{"key does not match certificate", func(e map[string][]byte) { e[EntryGuestKey] = other.keyPEM }, "guest key pair"},
		{"two host certificates", func(e map[string][]byte) { e[EntryHostCert] = append(e[EntryHostCert], e[EntryHostCert]...) }, "exactly one PEM"},
		{"host certificate not PEM", func(e map[string][]byte) { e[EntryHostCert] = []byte("nope") }, "exactly one PEM"},
		{"short scope", func(e map[string][]byte) { e[EntryScope] = []byte("ap-southeast-2/000000000001/fn-1") }, "want 5 segments"},
		{"address without port", func(e map[string][]byte) { e[EntryHostAddr] = []byte("10.15.8.1") }, "host address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEntries(t)
			tc.edit(e)
			_, err := LoadIdentity(writeEntries(t, e))
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestParseScope(t *testing.T) {
	_, err := ParseScope(string(gen1))
	require.NoError(t, err)
	for _, bad := range []string{
		"", "a/b/c/d", "a/b/c/d/e/f", "a//c/d/e", "a/b/c/d/e/", "a/b/c/d/e?x", "a/b/c/d/e#x", "a b/b/c/d/e",
	} {
		_, err := ParseScope(bad)
		assert.Error(t, err, bad)
	}
}
