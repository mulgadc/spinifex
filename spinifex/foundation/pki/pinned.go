package pki

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
)

// ErrPeerIdentity is returned when a pinned peer certificate does not carry
// the expected URI SAN.
var ErrPeerIdentity = errors.New("pki: peer identity mismatch")

// VerifyPinned requires chain to be exactly the pinned certificate, valid now
// for usage, whose only URI SAN is wantURI. Trusting the pin as a root alone
// would also accept other leaves it signed, so the DER is compared first.
func VerifyPinned(chain []*x509.Certificate, pinned *x509.Certificate, usage x509.ExtKeyUsage, wantURI string) error {
	if pinned == nil {
		return errors.New("pki: no pinned peer certificate")
	}
	if len(chain) != 1 {
		return fmt.Errorf("pki: peer sent %d certificates, want exactly the pinned one", len(chain))
	}
	leaf := chain[0]
	if !bytes.Equal(leaf.Raw, pinned.Raw) {
		return errors.New("pki: peer certificate is not the pinned certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(pinned)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return fmt.Errorf("pki: peer certificate: %w", err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != wantURI {
		return fmt.Errorf("%w: got %v, want %s", ErrPeerIdentity, leaf.URIs, wantURI)
	}
	return nil
}
