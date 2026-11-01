package workerkey

import (
	"context"
	"errors"

	"github.com/nats-io/nkeys"
)

// Claimer is the SQS domain's first-store decision for this worker identity.
type Claimer interface {
	// Claim returns createKey, or the one public key the store authorizes.
	Claim(ctx context.Context) (createKey bool, publicKey string, err error)
	RecordPublicKey(ctx context.Context, publicKey string) error
}

// Acquire returns the worker's store key under a current domain decision. A
// new key is usable only once its public key is recorded; a recorded key is
// never replaced, so a missing or different key on disk fails closed.
func (s Store) Acquire(ctx context.Context, c Claimer) (nkeys.KeyPair, error) {
	create, authorized, err := c.Claim(ctx)
	if err != nil {
		return nil, err
	}
	if !create {
		return s.ForExistingStore(authorized)
	}
	if authorized != "" {
		return nil, errors.New("first-store decision is contradictory")
	}
	// After a crash before recording, this loads the key already on disk.
	kp, err := s.ForNewStore()
	if err != nil {
		return nil, err
	}
	// The receipt key is made under the same decision, before the record
	// that ends this branch, so a recorded store always has one on disk.
	err = s.ensureReceipt()
	var pub string
	if err == nil {
		pub, err = kp.PublicKey()
	}
	if err == nil {
		err = c.RecordPublicKey(ctx, pub)
	}
	if err != nil {
		kp.Wipe()
		return nil, err
	}
	return kp, nil
}
