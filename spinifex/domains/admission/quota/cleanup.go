package quota

import (
	"context"
	"errors"
	"fmt"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
)

// OverrideCleanupDisposition is CleanupAccount's outcome for an account's
// quota override key.
type OverrideCleanupDisposition string

const (
	OverrideCleanupDeleted OverrideCleanupDisposition = "deleted"
	OverrideCleanupAbsent  OverrideCleanupDisposition = "absent"
	OverrideCleanupFailed  OverrideCleanupDisposition = "failed"
)

// evidenceRevisionGuardedDelete and evidenceObservedAbsent are the fixed
// evidence bases CleanupAccount reports; see kvstore/bucket.go:112-120 for why
// a plain read cannot support a stronger absence claim.
const (
	evidenceRevisionGuardedDelete = "revision-guarded-delete"
	evidenceObservedAbsent        = "observed-absent-at-scan"
)

// cleanupMaxAttempts bounds retries against a revision that keeps moving
// under concurrent writes, so cleanup fails closed instead of spinning.
const cleanupMaxAttempts = 5

// OverrideCleanup is CleanupAccount's result: what happened to an account's
// quota override key, and the fixed evidence basis for that claim.
type OverrideCleanup struct {
	Disposition OverrideCleanupDisposition
	Evidence    string
	Revision    uint64
}

// CleanupAccount removes accountID's quota override key so a reused account ID
// does not inherit a deleted account's raised or lowered limits. It is
// unwired: nothing calls it yet.
func (s *Service) CleanupAccount(ctx context.Context, accountID string) (*OverrideCleanup, error) {
	if accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "accountID is required for quota override cleanup")
	}
	if !awsidentifiers.IsAccountID(accountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "accountID %q is not a valid AWS account ID", accountID)
	}
	if accountID == awsidentifiers.GlobalAccountID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "quota override cleanup must not target the global account")
	}
	if s == nil || s.overrides == nil {
		return &OverrideCleanup{Disposition: OverrideCleanupFailed}, errors.New("quota: override bucket not configured")
	}

	// accountID is never resolved through any fallback here: cleanup must act
	// on exactly accountID and never fall back to the global key.
	for range cleanupMaxAttempts {
		entry, err := s.overrides.Get(ctx, accountID)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return &OverrideCleanup{Disposition: OverrideCleanupAbsent, Evidence: evidenceObservedAbsent}, nil
			}
			return &OverrideCleanup{Disposition: OverrideCleanupFailed}, fmt.Errorf("get quota override for cleanup: %w", err)
		}
		rev := entry.Revision()

		err = s.overrides.Delete(ctx, accountID, jetstream.LastRevision(rev))
		switch {
		case err == nil:
			return &OverrideCleanup{
				Disposition: OverrideCleanupDeleted,
				Evidence:    evidenceRevisionGuardedDelete,
				Revision:    rev,
			}, nil
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return &OverrideCleanup{Disposition: OverrideCleanupAbsent, Evidence: evidenceObservedAbsent}, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			// A concurrent write landed between the read and the guarded
			// delete; retry against whatever revision now holds.
			continue
		default:
			return &OverrideCleanup{Disposition: OverrideCleanupFailed}, fmt.Errorf("delete quota override for cleanup: %w", err)
		}
	}

	return &OverrideCleanup{Disposition: OverrideCleanupFailed},
		fmt.Errorf("quota override cleanup for account: exhausted %d attempts against concurrent writes", cleanupMaxAttempts)
}
