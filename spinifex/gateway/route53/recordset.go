package gateway_route53

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// ChangeResourceRecordSets is wired live in Sprint 1c. Single ChangeBatch
// per call, per-zone NATS queue group serializes writes on the daemon
// side (route53-v0.md R5 / D2).
func ChangeResourceRecordSets(_ *nats.Conn, _ string, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// ListResourceRecordSets is wired live in Sprint 1c.
func ListResourceRecordSets(_ *nats.Conn, _ string, _ string, _ map[string][]string) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}
