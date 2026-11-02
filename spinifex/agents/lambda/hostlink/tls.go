package hostlink

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ALPNv1 names L-GUEST major version 1; a peer offering any other fails the handshake.
const ALPNv1 = "spinifex-lambda-guest/1"

// FwCfgDir is where the guest kernel exposes the executor's fw_cfg entries.
const FwCfgDir = "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/lambda"

// fw_cfg entry names under FwCfgDir.
const (
	EntryGuestKey  = "guest-key"
	EntryGuestCert = "guest-cert"
	EntryHostCert  = "host-cert"
	EntryScope     = "expect"
	EntryHostAddr  = "host-addr"
)

// Roles named as the last URI SAN path segment.
const (
	RoleHost  = "host"
	RoleGuest = "guest"
)

const uriScheme = "spinifex-lambda"

// ErrPeerIdentity is returned when a peer certificate does not name the
// expected environment, generation and role.
var ErrPeerIdentity = errors.New("hostlink: peer identity mismatch")

// Scope names one environment generation:
// Region/account/function ID/environment ID/generation.
type Scope string

// ParseScope accepts exactly five non-empty, slash-separated segments.
func ParseScope(s string) (Scope, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 {
		return "", fmt.Errorf("hostlink: scope %q: want 5 segments, got %d", s, len(parts))
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\r\n?#%") {
			return "", fmt.Errorf("hostlink: scope %q: empty or invalid segment", s)
		}
	}
	return Scope(s), nil
}

// URI is the URI SAN a certificate for role in this scope must carry.
func (s Scope) URI(role string) string {
	return uriScheme + "://" + string(s) + "/" + role
}

// Identity is the guest's material for one environment generation.
type Identity struct {
	Cert     tls.Certificate
	HostCert *x509.Certificate
	Scope    Scope
	HostAddr string
}

// LoadIdentity reads the executor's fw_cfg entries from dir.
func LoadIdentity(dir string) (Identity, error) {
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, name, "raw"))
		if err != nil {
			return nil, fmt.Errorf("hostlink: read %s: %w", name, err)
		}
		return b, nil
	}
	var id Identity
	keyPEM, err := read(EntryGuestKey)
	if err != nil {
		return id, err
	}
	certPEM, err := read(EntryGuestCert)
	if err != nil {
		return id, err
	}
	hostPEM, err := read(EntryHostCert)
	if err != nil {
		return id, err
	}
	scope, err := read(EntryScope)
	if err != nil {
		return id, err
	}
	addr, err := read(EntryHostAddr)
	if err != nil {
		return id, err
	}

	if id.Cert, err = tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return Identity{}, fmt.Errorf("hostlink: guest key pair: %w", err)
	}
	blk, rest := pem.Decode(hostPEM)
	if blk == nil || blk.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return Identity{}, errors.New("hostlink: host certificate: want exactly one PEM certificate")
	}
	if id.HostCert, err = x509.ParseCertificate(blk.Bytes); err != nil {
		return Identity{}, fmt.Errorf("hostlink: host certificate: %w", err)
	}
	if id.Scope, err = ParseScope(strings.TrimSpace(string(scope))); err != nil {
		return Identity{}, err
	}
	id.HostAddr = strings.TrimSpace(string(addr))
	if _, _, err := net.SplitHostPort(id.HostAddr); err != nil {
		return Identity{}, fmt.Errorf("hostlink: host address: %w", err)
	}
	return id, nil
}

// VerifyPeer requires the peer to present exactly the pinned certificate,
// validates it, and requires its only URI SAN to be want. Hostname checks do
// not cover URI SANs, so both ends call this rather than relying on ServerName.
func VerifyPeer(cs tls.ConnectionState, pinned *x509.Certificate, usage x509.ExtKeyUsage, want string) error {
	if pinned == nil {
		return errors.New("hostlink: no pinned peer certificate")
	}
	if cs.Version != tls.VersionTLS13 {
		return fmt.Errorf("hostlink: TLS version %#x, want 1.3", cs.Version)
	}
	if cs.NegotiatedProtocol != ALPNv1 {
		return fmt.Errorf("hostlink: application protocol %q, want %q", cs.NegotiatedProtocol, ALPNv1)
	}
	if len(cs.PeerCertificates) != 1 {
		return fmt.Errorf("hostlink: peer sent %d certificates, want exactly the pinned one", len(cs.PeerCertificates))
	}
	leaf := cs.PeerCertificates[0]
	if !bytes.Equal(leaf.Raw, pinned.Raw) {
		return errors.New("hostlink: peer certificate is not the pinned certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(pinned)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return fmt.Errorf("hostlink: peer certificate: %w", err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != want {
		return fmt.Errorf("%w: got %v, want %s", ErrPeerIdentity, leaf.URIs, want)
	}
	return nil
}

// ClientConfig is the guest's TLS 1.3 configuration. Certificate checking is
// done in VerifyConnection by VerifyPeer, since the host has no DNS name.
func (id Identity) ClientConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.Cert},
		NextProtos:         []string{ALPNv1},
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection performs full validation against the pinned host certificate.
		VerifyConnection: func(cs tls.ConnectionState) error {
			return VerifyPeer(cs, id.HostCert, x509.ExtKeyUsageServerAuth, id.Scope.URI(RoleHost))
		},
	}
}

// Dial opens the authenticated control connection to the host. Under TLS 1.3
// the host may still refuse the guest's certificate after Dial returns; that
// surfaces on the first read, so the caller must not treat Dial as acceptance.
func Dial(ctx context.Context, id Identity) (*tls.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: id.ClientConfig()}
	c, err := d.DialContext(ctx, "tcp", id.HostAddr)
	if err != nil {
		return nil, fmt.Errorf("hostlink: dial host %s: %w", id.HostAddr, err)
	}
	tc, ok := c.(*tls.Conn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("hostlink: dial host %s: unexpected connection type %T", id.HostAddr, c)
	}
	return tc, nil
}
