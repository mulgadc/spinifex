package uefistore

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
)

// ClaimPrefix is the kvsync prefix of every store's claim. An instance's claim
// is ClaimPrefix+instanceID, and names the node that may hold variable changes
// no object has yet.
const ClaimPrefix = "uefi."

// NewClaims returns node's group of store claims over store. The daemon builds
// it once per process, as kvsync requires of every prefix.
func NewClaims(store kvsync.KVStore, node string) *kvsync.ClaimGroup {
	return kvsync.NewClaimGroup(store, ClaimPrefix, node)
}

// ErrFenced is a stamp or pointer move refused because the record carries a
// claim generation other than the caller's: a later acquisition has stamped
// it, so the caller no longer owns the store and must change nothing.
var ErrFenced = errors.New("uefistore: store record is fenced to another claim generation")

// fenceAttempts bounds the read-then-write loops of Stamp and MovePointer. A
// lost race is re-read, so a concurrent stamp is seen and refused, not retried.
const fenceAttempts = 5

// Stamp raises instanceID's record to generation, the claim token the caller
// has just acquired or adopted, and returns the record it stamped, whose
// pointer the caller boots from. One above generation is ErrFenced, and one
// deleting is ErrDeleting. A record already at generation is still written by
// CAS, because a follower can answer the read with an older pointer.
func (s *Store) Stamp(ctx context.Context, instanceID string, generation uint64) (*UEFIStoreRecord, error) {
	if generation == 0 {
		return nil, fmt.Errorf("uefistore: stamp %s: generation 0 is no claim", instanceID)
	}
	for range fenceAttempts {
		rec, rev, err := s.Get(ctx, instanceID)
		if err != nil {
			return nil, err
		}
		switch {
		case rec.State == StateDeleting:
			return nil, fmt.Errorf("%w: %s", ErrDeleting, instanceID)
		case rec.Generation > generation:
			return nil, fmt.Errorf("%w: %s is at generation %d, above the claim's %d", ErrFenced, instanceID, rec.Generation, generation)
		}
		rec.Generation = generation
		_, err = s.CompareAndSet(ctx, rec, rev)
		if err == nil {
			return rec, nil
		}
		if !errors.Is(err, kvstore.ErrConflict) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("uefistore: stamp %s: %w after %d attempts", instanceID, kvstore.ErrConflict, fenceAttempts)
}

// MovePointer points instanceID's record at ref only while the record is at
// generation. A record at any other generation is ErrFenced and is left
// unchanged. Moving to the key the record already points at writes nothing.
func (s *Store) MovePointer(ctx context.Context, instanceID string, generation uint64, ref UEFIVarStoreRef) error {
	if err := ref.validate(instanceID); err != nil {
		return fmt.Errorf("%w: move %s: %w", ErrInvalidRecord, instanceID, err)
	}
	for range fenceAttempts {
		rec, rev, err := s.Get(ctx, instanceID)
		if err != nil {
			return err
		}
		switch {
		case rec.State == StateDeleting:
			return fmt.Errorf("%w: %s", ErrDeleting, instanceID)
		case rec.Generation != generation:
			return fmt.Errorf("%w: %s is at generation %d, and the writer holds %d", ErrFenced, instanceID, rec.Generation, generation)
		case rec.Pointer != nil && rec.Pointer.Key == ref.Key:
			return nil
		}
		rec.Pointer = &ref
		_, err = s.CompareAndSet(ctx, rec, rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, kvstore.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("uefistore: move %s: %w after %d attempts", instanceID, kvstore.ErrConflict, fenceAttempts)
}
