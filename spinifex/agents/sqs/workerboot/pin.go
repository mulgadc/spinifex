package workerboot

import (
	"crypto/tls"
	"errors"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
)

// ErrStoreIdentity is a store whose TLS server key does not match the pin
// the gateway recorded for it; nothing is sent to it.
var ErrStoreIdentity = errors.New("the store did not prove its pinned server key")

// pinnedTLS trusts exactly the server key named by pin. The certificate
// chain is not used: the pin, recorded by the bound store, is the identity.
func pinnedTLS(pin string) (*tls.Config, error) {
	if !sqsv1.ValidPin(pin) {
		return nil, errors.New("store server key pin is not valid")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The chain is replaced by the pin check in VerifyConnection.
		InsecureSkipVerify: true, //nolint:gosec // pinned below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || sqsv1.Pin(cs.PeerCertificates[0]) != pin {
				return ErrStoreIdentity
			}
			return nil
		},
	}, nil
}
