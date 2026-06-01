package gateway_route53

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// CreateHostedZone is wired live in Sprint 1b. The 1a stub returns
// NotImplemented so callers exercising route lookup get a proper
// awserrors-shaped reply instead of a generic 404.
func CreateHostedZone(_ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// GetHostedZone is wired live in Sprint 1b.
func GetHostedZone(_ *nats.Conn, _ string, _ string) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// ListHostedZones is wired live in Sprint 1b.
func ListHostedZones(_ *nats.Conn, _ string) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// UpdateHostedZoneComment is wired live in Sprint 1b.
func UpdateHostedZoneComment(_ *nats.Conn, _ string, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// DeleteHostedZone is wired live in Sprint 1b.
func DeleteHostedZone(_ *nats.Conn, _ string, _ string) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}
