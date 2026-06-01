package gateway_route53

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// GetChange is wired live in Sprint 1c. PENDING/INSYNC transitions key
// off the per-zone loaded-version tracker (D2 in route53-v0.md).
func GetChange(_ *nats.Conn, _ string, _ string) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}
