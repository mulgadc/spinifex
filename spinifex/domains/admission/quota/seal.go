package quota

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// sealMaxAttempts bounds SealAccountOverrides' retry loop against a revision
// that keeps moving under concurrent writes, so it fails closed instead of
// spinning.
const sealMaxAttempts = 5

// sealedRecord is the fixed value a seal writes. No dimension or provenance
// survives, so no tenant commercial setting outlives the seal.
var sealedRecord = &overrideRecord{State: OverrideStateSealed}

// OverrideSeal is SealAccountOverrides' result: the revision at which the
// stream leader accepted the sealed value.
type OverrideSeal struct {
	Revision uint64
}

// AccountTerminationChecker reports whether IAM holds accountID as
// TERMINATING. A missing record is (false, nil); only a failed read is an
// error.
type AccountTerminationChecker interface {
	AccountTerminating(ctx context.Context, accountID string) (bool, error)
}

// SetAccountTerminationChecker wires the seal precondition; until it is set,
// SealAccountOverrides refuses.
func (s *Service) SetAccountTerminationChecker(c AccountTerminationChecker) {
	if s == nil {
		return
	}
	s.terminationChecker = c
}

// checkTerminating runs before any override-KV call, so a refusal never
// writes or opens the bucket. Not TERMINATING (incl. missing) is
// IncorrectState; a read failure or unwired checker is InternalError.
func (s *Service) checkTerminating(ctx context.Context, accountID string) error {
	if s.terminationChecker == nil {
		slog.ErrorContext(ctx, "Quota override seal has no termination checker wired", "accountID", accountID)
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

// isRevisionConflict reports a lost CAS race to retry, including the bare
// wrong-last-sequence APIError from Create's retry against a delete marker.
// A private copy of domains/ec2/account's helper, which this package may not import.
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

// sealBucket returns this gateway's override bucket, or get-or-creates it
// when quotas are disabled here. Called only after checkTerminating accepts,
// so a refused seal never creates the bucket.
func (s *Service) sealBucket(ctx context.Context) (jetstream.KeyValue, error) {
	if s.overrides != nil {
		return s.overrides, nil
	}
	if s.js == nil {
		slog.ErrorContext(ctx, "Quota override seal has no JetStream client wired to open the override bucket")
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, s.js, KVBucketAccountQuota, 1)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to open quota override bucket for seal", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return kv, nil
}

// SealAccountOverrides seals accountID's override record so no later writer
// can recreate it. Success is only a leader-accepted write, evidenced by the
// revision; a missing key is created sealed, never reported from absence.
func (s *Service) SealAccountOverrides(ctx context.Context, accountID string) (*OverrideSeal, error) {
	if accountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "accountID is required to seal quota overrides")
	}
	if !awsidentifiers.IsAccountID(accountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "accountID %q is not a valid AWS account ID", accountID)
	}
	if accountID == awsidentifiers.GlobalAccountID {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "quota override seal must not target the global account")
	}

	if err := s.checkTerminating(ctx, accountID); err != nil {
		return nil, err
	}

	overrides, err := s.sealBucket(ctx)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(sealedRecord)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to encode sealed quota override record", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// accountID is never resolved through any fallback here: a seal must act
	// on exactly accountID and never fall back to the global key.
	for range sealMaxAttempts {
		var rev uint64
		entry, getErr := overrides.Get(ctx, accountID)
		switch {
		case getErr == nil:
			rev = entry.Revision()
		case errors.Is(getErr, jetstream.ErrKeyNotFound):
			rev = 0
		default:
			slog.ErrorContext(ctx, "Failed to read quota override for seal", "accountID", accountID, "err", getErr)
			return nil, errors.New(awserrors.ErrorInternalError)
		}

		var newRev uint64
		var writeErr error
		if rev == 0 {
			newRev, writeErr = overrides.Create(ctx, accountID, data)
		} else {
			newRev, writeErr = overrides.Update(ctx, accountID, data, rev)
		}

		switch {
		case writeErr == nil:
			return &OverrideSeal{Revision: newRev}, nil
		case isRevisionConflict(writeErr):
			continue
		default:
			slog.ErrorContext(ctx, "Failed to write sealed quota override record", "accountID", accountID, "err", writeErr)
			return nil, errors.New(awserrors.ErrorInternalError)
		}
	}

	slog.ErrorContext(ctx, "Quota override seal exhausted attempts against concurrent writes", "accountID", accountID, "attempts", sealMaxAttempts)
	return nil, errors.New(awserrors.ErrorInternalError)
}
