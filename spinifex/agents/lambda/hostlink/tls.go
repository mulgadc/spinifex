package hostlink

import (
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

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/pki"
)

// FwCfgDir is where the guest kernel exposes the executor's fw_cfg entries.
const FwCfgDir = FwCfgRoot + "/by_name/" + lambdav1.FwCfgPrefix

// Identity is the guest's material for one environment generation.
type Identity struct {
	Cert     tls.Certificate
	HostCert *x509.Certificate
	Scope    lambdav1.Scope
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
	keyPEM, err := read(lambdav1.EntryGuestKey)
	if err != nil {
		return id, err
	}
	certPEM, err := read(lambdav1.EntryGuestCert)
	if err != nil {
		return id, err
	}
	hostPEM, err := read(lambdav1.EntryHostCert)
	if err != nil {
		return id, err
	}
	scope, err := read(lambdav1.EntryScope)
	if err != nil {
		return id, err
	}
	addr, err := read(lambdav1.EntryHostAddr)
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
	if id.Scope, err = lambdav1.ParseScope(strings.TrimSpace(string(scope))); err != nil {
		return Identity{}, err
	}
	id.HostAddr = strings.TrimSpace(string(addr))
	if _, _, err := net.SplitHostPort(id.HostAddr); err != nil {
		return Identity{}, fmt.Errorf("hostlink: host address: %w", err)
	}
	return id, nil
}

// ClientConfig is the guest's TLS 1.3 configuration. The host has no DNS name,
// so VerifyConnection checks the pinned host certificate and its URI SAN.
func (id Identity) ClientConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.Cert},
		NextProtos:         []string{Protocol},
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection performs full validation against the pinned host certificate.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.NegotiatedProtocol != Protocol {
				return fmt.Errorf("hostlink: application protocol %q, want %q", cs.NegotiatedProtocol, Protocol)
			}
			return pki.VerifyPinned(cs.PeerCertificates, id.HostCert, x509.ExtKeyUsageServerAuth, id.Scope.URI(lambdav1.RoleHost))
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
