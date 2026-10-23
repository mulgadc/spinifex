package account

import (
	"context"
	"errors"
	"fmt"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountSettingsCleanupDisposition is CleanupAccount's outcome for an
// account's settings key.
type AccountSettingsCleanupDisposition string

const (
	AccountSettingsCleanupDeleted AccountSettingsCleanupDisposition = "deleted"
	AccountSettingsCleanupAbsent  AccountSettingsCleanupDisposition = "absent"
	AccountSettingsCleanupFailed  AccountSettingsCleanupDisposition = "failed"
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

// AccountSettingsCleanup is CleanupAccount's result: what happened to an
// account's settings key, and the fixed evidence basis for that claim.
type AccountSettingsCleanup struct {
	Disposition AccountSettingsCleanupDisposition
	Evidence    string
	Revision    uint64
}

// CleanupAccount removes accountID's settings key so a reused account ID does
// not inherit a deleted account's EBS-encryption-by-default or
// serial-console-access settings. It is unwired: nothing calls it yet.
func (s *AccountSettingsServiceImpl) CleanupAccount(ctx context.Context, accountID string) (*AccountSettingsCleanup, error) {
	if accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "accountID is required for settings cleanup")
	}
	if !awsidentifiers.IsAccountID(accountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "accountID %q is not a valid AWS account ID", accountID)
	}
	if accountID == awsidentifiers.GlobalAccountID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "settings cleanup must not target the global account")
	}

	// settingsKey is never used here: cleanup must act on exactly accountID
	// and never fall back to the global key.
	for range cleanupMaxAttempts {
		entry, err := s.settingsKV.Get(ctx, accountID)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return &AccountSettingsCleanup{Disposition: AccountSettingsCleanupAbsent, Evidence: evidenceObservedAbsent}, nil
			}
			return &AccountSettingsCleanup{Disposition: AccountSettingsCleanupFailed}, fmt.Errorf("get account settings for cleanup: %w", err)
		}
		rev := entry.Revision()

		err = s.settingsKV.Delete(ctx, accountID, jetstream.LastRevision(rev))
		switch {
		case err == nil:
			return &AccountSettingsCleanup{
				Disposition: AccountSettingsCleanupDeleted,
				Evidence:    evidenceRevisionGuardedDelete,
				Revision:    rev,
			}, nil
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return &AccountSettingsCleanup{Disposition: AccountSettingsCleanupAbsent, Evidence: evidenceObservedAbsent}, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			// A concurrent write landed between the read and the guarded
			// delete; retry against whatever revision now holds.
			continue
		default:
			return &AccountSettingsCleanup{Disposition: AccountSettingsCleanupFailed}, fmt.Errorf("delete account settings for cleanup: %w", err)
		}
	}

	return &AccountSettingsCleanup{Disposition: AccountSettingsCleanupFailed},
		fmt.Errorf("settings cleanup for account: exhausted %d attempts against concurrent writes", cleanupMaxAttempts)
}
