package ochre

import (
	"context"
	"errors"
	"time"
)

// HealthHold protects a READY endpoint from automatic reclaim and capacity
// eviction while its health is in question. Token and Epoch name the sweep
// operation that reserved it, once sweeps carry durable tokens.
type HealthHold struct {
	Token      string    `json:"token,omitempty"`
	Epoch      uint64    `json:"epoch,omitempty"`
	ReservedAt time.Time `json:"reserved_at"`
}

var (
	errHoldNotReady = errors.New("bedrock: health hold applies only to a READY endpoint")
	errHoldExists   = errors.New("bedrock: endpoint already carries a health hold")
	errHoldAbsent   = errors.New("bedrock: endpoint carries no health hold")
)

// Held reports whether r carries a health hold.
func (r EndpointRecord) Held() bool {
	return r.HealthHold != nil
}

// reserveHealthHold adds hold to observed, the READY record read at rev, by a
// single CAS at rev. A conflict is kvstore.ErrConflict; any other error leaves
// the outcome unknown, and neither is retried.
func (s *Service) reserveHealthHold(ctx context.Context, key string, observed EndpointRecord, rev uint64, hold HealthHold) (uint64, error) {
	switch {
	case observed.State != StateReady:
		return 0, errHoldNotReady
	case observed.Held():
		return 0, errHoldExists
	}
	next := observed
	next.HealthHold = &hold
	next.Generation = observed.Generation + 1
	return s.writeHold(ctx, key, next, rev)
}

// clearHealthHold removes the hold from observed, the held READY record read at
// rev, by a single CAS at rev, with the same outcomes as reserveHealthHold. A
// record past READY belongs to its delete or launch and is never rewritten.
func (s *Service) clearHealthHold(ctx context.Context, key string, observed EndpointRecord, rev uint64) (uint64, error) {
	switch {
	case observed.State != StateReady:
		return 0, errHoldNotReady
	case !observed.Held():
		return 0, errHoldAbsent
	}
	next := observed
	next.HealthHold = nil
	next.Generation = observed.Generation + 1
	return s.writeHold(ctx, key, next, rev)
}

func (s *Service) writeHold(ctx context.Context, key string, next EndpointRecord, rev uint64) (uint64, error) {
	write := func() (uint64, error) { return s.store.CompareAndSet(ctx, key, &next, rev) }
	if s.holdWrite != nil {
		return s.holdWrite(write)
	}
	return write()
}
