package account

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
)

// sealMaxAttempts bounds SealAccount's retry loop against a revision that
// keeps moving under concurrent writes, so it fails closed instead of
// spinning.
const sealMaxAttempts = 5

// sealedRecord is the fixed value a seal writes. Clearing both booleans
// retains no tenant setting; an older reader that ignores state sees the
// AWS defaults.
var sealedRecord = &AccountSettingsRecord{State: AccountSettingsStateSealed}

// AccountSettingsSeal is SealAccount's result: the revision at which the
// stream leader accepted the sealed value.
type AccountSettingsSeal struct {
	Revision uint64
}

// AccountTerminationChecker reports whether IAM holds accountID as TERMINATING.
// A missing record is (false, nil); only a failed read is an error.
type AccountTerminationChecker interface {
	AccountTerminating(ctx context.Context, accountID string) (bool, error)
}

// SetAccountTerminationChecker wires the precondition; until it is set, SealAccount refuses.
func (s *AccountSettingsServiceImpl) SetAccountTerminationChecker(c AccountTerminationChecker) {
	s.terminationChecker = c
}

// checkTerminating runs before any settings-KV call, so a refusal never writes.
// Not TERMINATING, including a missing IAM record, is IncorrectState; a read
// failure or an unwired checker is InternalError.
func (s *AccountSettingsServiceImpl) checkTerminating(ctx context.Context, accountID string) error {
	if s.terminationChecker == nil {
		slog.ErrorContext(ctx, "Account settings seal has no termination checker wired", "accountID", accountID)
		return errors.New(awserrors.ErrorInternalError)
	}

	terminating, err := s.terminationChecker.AccountTerminating(ctx, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to read account status for seal precondition", "accountID", accountID, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
	if !terminating {
		return awserrors.Errorf(awserrors.ErrorIncorrectState, "account %s is not TERMINATING", accountID)
	}
	return nil
}

// isRevisionConflict reports a lost CAS race SealAccount should retry. Besides
// the two sentinels, Create's retry against a delete marker returns a bare
// *jetstream.APIError carrying a wrong-last-sequence code.
func isRevisionConflict(err error) bool {
	if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return true
	}
	if apiErr, ok := errors.AsType[*jetstream.APIError](err); ok {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
			apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant
	}
	return false
}

// SealAccount seals accountID's settings record so no later mutation can
// recreate it. It never relies on a read for its claim: the only success is a
// leader-accepted write, evidenced by the returned revision.
func (s *AccountSettingsServiceImpl) SealAccount(ctx context.Context, accountID string) (*AccountSettingsSeal, error) {
	if accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "accountID is required to seal settings")
	}
	if !awsidentifiers.IsAccountID(accountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "accountID %q is not a valid AWS account ID", accountID)
	}
	if accountID == awsidentifiers.GlobalAccountID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "settings seal must not target the global account")
	}

	if err := s.checkTerminating(ctx, accountID); err != nil {
		return nil, err
	}

	data, err := json.Marshal(sealedRecord)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to encode sealed account settings record", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// settingsKey is never used here: a seal must act on exactly accountID
	// and never fall back to the global key.
	for range sealMaxAttempts {
		var rev uint64
		entry, getErr := s.settingsKV.Get(ctx, accountID)
		switch {
		case getErr == nil:
			rev = entry.Revision()
		case errors.Is(getErr, jetstream.ErrKeyNotFound):
			rev = 0
		default:
			slog.ErrorContext(ctx, "Failed to read account settings for seal", "accountID", accountID, "err", getErr)
			return nil, errors.New(awserrors.ErrorInternalError)
		}

		var newRev uint64
		var writeErr error
		if rev == 0 {
			newRev, writeErr = s.settingsKV.Create(ctx, accountID, data)
		} else {
			newRev, writeErr = s.settingsKV.Update(ctx, accountID, data, rev)
		}

		switch {
		case writeErr == nil:
			return &AccountSettingsSeal{Revision: newRev}, nil
		case isRevisionConflict(writeErr):
			continue
		default:
			slog.ErrorContext(ctx, "Failed to write sealed account settings record", "accountID", accountID, "err", writeErr)
			return nil, errors.New(awserrors.ErrorInternalError)
		}
	}

	slog.ErrorContext(ctx, "Account settings seal exhausted attempts against concurrent writes", "accountID", accountID, "attempts", sealMaxAttempts)
	return nil, errors.New(awserrors.ErrorInternalError)
}
