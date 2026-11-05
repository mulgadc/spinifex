// Package guestconn is the host end of the Lambda guest control connection:
// per-generation identities and an authenticating TCP accept loop.
package guestconn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/foundation/pki"
)

// Generation is the host's material for one environment generation: its own
// identity and the one guest certificate it accepts. Nothing is reused.
type Generation struct {
	Scope     lambdav1.Scope
	host      tls.Certificate
	hostPEM   []byte
	guestCert *x509.Certificate
	guestPEM  []byte
	guestKey  []byte
}

// NewGeneration creates fresh self-signed host and guest identities for
// scope, valid from a minute ago for ttl.
func NewGeneration(scope lambdav1.Scope, ttl time.Duration) (*Generation, error) {
	if _, err := lambdav1.ParseScope(string(scope)); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, errors.New("guestconn: generation ttl must be positive")
	}
	hostPEM, hostKey, _, err := selfSigned(scope, lambdav1.RoleHost, ttl)
	if err != nil {
		return nil, err
	}
	host, err := tls.X509KeyPair(hostPEM, hostKey)
	if err != nil {
		return nil, fmt.Errorf("guestconn: host key pair: %w", err)
	}
	guestPEM, guestKey, guestCert, err := selfSigned(scope, lambdav1.RoleGuest, ttl)
	if err != nil {
		return nil, err
	}
	return &Generation{Scope: scope, host: host, hostPEM: hostPEM, guestCert: guestCert, guestPEM: guestPEM, guestKey: guestKey}, nil
}

func selfSigned(scope lambdav1.Scope, role string, ttl time.Duration) (certPEM, keyPEM []byte, cert *x509.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s key: %w", role, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s serial: %w", role, err)
	}
	u, err := url.Parse(scope.URI(role))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s URI: %w", role, err)
	}
	usage := x509.ExtKeyUsageServerAuth
	if role == lambdav1.RoleGuest {
		usage = x509.ExtKeyUsageClientAuth
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "spinifex-lambda " + role},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(ttl),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true, URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s certificate: %w", role, err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s key encoding: %w", role, err)
	}
	if cert, err = x509.ParseCertificate(der); err != nil {
		return nil, nil, nil, fmt.Errorf("guestconn: %s certificate: %w", role, err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), cert, nil
}

// GuestEntries returns the fw_cfg entries the guest needs, by entry name. The
// guest key is secret: the caller writes these 0600 and deletes them once
// QEMU has read them.
func (g *Generation) GuestEntries(hostAddr string) map[string][]byte {
	return map[string][]byte{
		lambdav1.EntryGuestKey:  append([]byte(nil), g.guestKey...),
		lambdav1.EntryGuestCert: append([]byte(nil), g.guestPEM...),
		lambdav1.EntryHostCert:  append([]byte(nil), g.hostPEM...),
		lambdav1.EntryScope:     []byte(g.Scope),
		lambdav1.EntryHostAddr:  []byte(hostAddr),
	}
}

// ServerConfig is the host's TLS 1.3 configuration: it requires a client
// certificate and accepts only this generation's guest certificate.
func (g *Generation) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{g.host},
		NextProtos:   []string{hostlink.Protocol},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.NegotiatedProtocol != hostlink.Protocol {
				return fmt.Errorf("guestconn: application protocol %q, want %q", cs.NegotiatedProtocol, hostlink.Protocol)
			}
			return pki.VerifyPinned(cs.PeerCertificates, g.guestCert, x509.ExtKeyUsageClientAuth, g.Scope.URI(lambdav1.RoleGuest))
		},
	}
}

// DeadlineListener is a listener whose Accept can be interrupted.
type DeadlineListener interface {
	net.Listener
	SetDeadline(t time.Time) error
}

// AcceptGuest returns the first connection that authenticates as this
// generation's guest. Peers on the untrusted network may connect first; each
// gets handshakeTimeout and is closed on failure. It stops when ctx ends.
func (g *Generation) AcceptGuest(ctx context.Context, ln DeadlineListener, handshakeTimeout time.Duration) (*tls.Conn, error) {
	if handshakeTimeout <= 0 {
		return nil, errors.New("guestconn: handshake timeout must be positive")
	}
	_ = ln.SetDeadline(time.Time{})
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = ln.SetDeadline(time.Now())
		close(fired)
	})
	// If the callback started, wait for it so its expired deadline cannot
	// land after the clear and leave the listener unusable.
	defer func() {
		if !stop() {
			<-fired
			_ = ln.SetDeadline(time.Time{})
		}
	}()
	cfg := g.ServerConfig()
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("guestconn: no authenticated guest: %w", context.Cause(ctx))
			}
			return nil, fmt.Errorf("guestconn: accept: %w", err)
		}
		if c := g.handshake(ctx, raw, cfg, handshakeTimeout); c != nil {
			return c, nil
		}
	}
}

func (g *Generation) handshake(ctx context.Context, raw net.Conn, cfg *tls.Config, timeout time.Duration) *tls.Conn {
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tc := tls.Server(raw, cfg)
	if err := tc.HandshakeContext(hctx); err != nil {
		slog.Warn("guestconn: refused control connection", "remote", raw.RemoteAddr().String(), "err", err)
		_ = raw.Close()
		return nil
	}
	return tc
}
