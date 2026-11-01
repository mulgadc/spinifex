package workerkey

import (
	"context"
	"errors"
	"fmt"
)

// Decision is the platform's receipt-key initialization state for this worker
// volume: Pending authorizes creating or resuming the key, otherwise exactly
// the recorded fingerprint is accepted.
type Decision struct {
	Pending     bool
	Fingerprint string
	// HostNodes is the gateway's declared cluster node count, zero if unknown.
	HostNodes int
}

// Claimer reads and completes the platform's receipt-key initialization.
type Claimer interface {
	Claim(ctx context.Context) (Decision, error)
	RecordFingerprint(ctx context.Context, fingerprint string) error
}

// ErrDiskHasData refuses a pending initialization on a disk that already holds
// SQS data: the platform and the disk disagree about whether it is new.
var ErrDiskHasData = errors.New("receipt key initialization is pending but the disk already holds SQS data")

// Acquire returns the receipt key under the current platform decision. A
// pending decision creates or resumes the key only while hasData reports no
// SQS data, then records its fingerprint; the caller must clear the key.
func (s Store) Acquire(ctx context.Context, c Claimer, hasData func() (bool, error)) ([]byte, error) {
	d, err := c.Claim(ctx)
	if err != nil {
		return nil, err
	}
	if !d.Pending {
		return s.ForRecorded(d.Fingerprint)
	}
	if d.Fingerprint != "" {
		return nil, errors.New("receipt key decision is contradictory")
	}
	data, err := hasData()
	if err != nil {
		return nil, fmt.Errorf("check the disk for SQS data: %w", err)
	}
	if data {
		return nil, ErrDiskHasData
	}
	key, err := s.ensureReceipt()
	if err != nil {
		return nil, err
	}
	if err := c.RecordFingerprint(ctx, Fingerprint(key)); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

// ForRecorded returns the disk's receipt key only if it matches fingerprint.
func (s Store) ForRecorded(fingerprint string) ([]byte, error) {
	if len(fingerprint) != 64 {
		return nil, errors.New("recorded receipt key fingerprint is malformed")
	}
	key, err := s.ReceiptKey()
	if err != nil {
		return nil, err
	}
	if Fingerprint(key) != fingerprint {
		clear(key)
		return nil, ErrKeyMismatch
	}
	return key, nil
}
