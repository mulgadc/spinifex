package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketAccountSettings        = "spinifex-ec2-account-settings"
	KVBucketAccountSettingsVersion = 1
)

// AccountSettingsState is a settings record's lifecycle state. "" is active;
// a record written before the state field existed has none and stays active.
type AccountSettingsState string

const AccountSettingsStateSealed AccountSettingsState = "sealed"

// AccountSettingsRecord represents stored account settings.
type AccountSettingsRecord struct {
	EbsEncryptionByDefault bool                 `json:"ebs_encryption_by_default"`
	SerialConsoleAccess    bool                 `json:"serial_console_access"`
	State                  AccountSettingsState `json:"state,omitempty"`
}

// AccountSettingsServiceImpl implements account settings operations with NATS JetStream persistence.
type AccountSettingsServiceImpl struct {
	config     *config.Config
	js         jetstream.JetStream
	settingsKV jetstream.KeyValue

	// terminationChecker is the seal precondition; SealAccount refuses until
	// it is wired.
	terminationChecker AccountTerminationChecker
}

var _ AccountSettingsService = (*AccountSettingsServiceImpl)(nil)

// NewAccountSettingsServiceImplWithNATS creates an account settings service with NATS JetStream for persistence.
func NewAccountSettingsServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*AccountSettingsServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	settingsKV, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketAccountSettings, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create account settings KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketAccountSettings, settingsKV, KVBucketAccountSettingsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketAccountSettings, err)
	}

	slog.Info("Account settings service initialized with JetStream KV", "bucket", KVBucketAccountSettings)

	return &AccountSettingsServiceImpl{
		config:     cfg,
		js:         js,
		settingsKV: settingsKV,
	}, nil
}

// settingsKey returns the per-account KV key for storing settings.
// Falls back to GlobalAccountID for pre-Phase-4 resources with no accountID.
func settingsKey(accountID string) string {
	if accountID == "" {
		return awsidentifiers.GlobalAccountID
	}
	return accountID
}

// errSettingsSealed marks a mutation or read that found a sealed record, so
// callers map it to Blocked without logging it as a fault.
var errSettingsSealed = errors.New(awserrors.ErrorBlocked)

// errSettingsContended marks a mutation whose CAS retries were exhausted
// under concurrent writers, bounding the retry loop instead of spinning.
var errSettingsContended = errors.New("account settings: CAS retries exhausted")

// checkActive rejects a sealed or unknown-state record; "" is active and
// passes through. A mutation must never act on a record that failed this.
func checkActive(rec *AccountSettingsRecord) error {
	switch rec.State {
	case "":
		return nil
	case AccountSettingsStateSealed:
		return errSettingsSealed
	default:
		return fmt.Errorf("account settings: unknown state %q", rec.State)
	}
}

// mapSettingsError turns a mutateSettings or getSettings failure into the AWS
// error a caller returns: sealed is an expected, unlogged Blocked; everything
// else is InternalError, logged with the account ID.
func mapSettingsError(ctx context.Context, accountID string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSettingsSealed):
		return errors.New(awserrors.ErrorBlocked)
	case errors.Is(err, errSettingsContended):
		slog.ErrorContext(ctx, "Account settings CAS retries exhausted under contention", "accountID", accountID)
	case errors.Is(err, kvutil.ErrRead):
		slog.ErrorContext(ctx, "Failed to read account settings from KV", "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrDecode):
		slog.ErrorContext(ctx, "Corrupt account settings record in KV", "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrEncode):
		slog.ErrorContext(ctx, "Failed to marshal account settings record", "accountID", accountID, "err", err)
	case errors.Is(err, kvutil.ErrWrite):
		slog.ErrorContext(ctx, "Failed to write account settings to KV", "accountID", accountID, "err", err)
	default:
		slog.ErrorContext(ctx, "Account settings operation failed", "accountID", accountID, "err", err)
	}
	return errors.New(awserrors.ErrorInternalError)
}

// mutateSettings applies set to accountID's record at the revision it read.
// changed is always true, so every success is a leader-accepted write over an
// active value; a seal landing between the read and the write fails the CAS.
func (s *AccountSettingsServiceImpl) mutateSettings(ctx context.Context, accountID string, set func(*AccountSettingsRecord)) error {
	_, err := kvutil.Update(ctx, s.settingsKV, settingsKey(accountID), kvutil.CASConfig{
		CreateIfAbsent: true,
		Exhausted:      func(string, int) error { return errSettingsContended },
	}, func(rec *AccountSettingsRecord) (bool, error) {
		if err := checkActive(rec); err != nil {
			return false, err
		}
		set(rec)
		return true, nil
	})
	return mapSettingsError(ctx, accountID, err)
}

// getSettings retrieves current account settings, rejecting a sealed or
// unknown-state record so a read never answers from behind a seal. Absent is
// the AWS defaults; a read is a direct get and can be stale.
func (s *AccountSettingsServiceImpl) getSettings(ctx context.Context, accountID string) (*AccountSettingsRecord, error) {
	entry, err := s.settingsKV.Get(ctx, settingsKey(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return &AccountSettingsRecord{}, nil
		}
		slog.ErrorContext(ctx, "Failed to read account settings from KV", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	var settings AccountSettingsRecord
	if err := json.Unmarshal(entry.Value(), &settings); err != nil {
		slog.ErrorContext(ctx, "Corrupt account settings record in KV", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if err := checkActive(&settings); err != nil {
		return nil, mapSettingsError(ctx, accountID, err)
	}

	return &settings, nil
}

// EnableEbsEncryptionByDefault enables EBS encryption by default for the account.
func (s *AccountSettingsServiceImpl) EnableEbsEncryptionByDefault(ctx context.Context, input *ec2.EnableEbsEncryptionByDefaultInput, accountID string) (*ec2.EnableEbsEncryptionByDefaultOutput, error) {
	slog.InfoContext(ctx, "EnableEbsEncryptionByDefault called", "accountID", accountID)

	if err := s.mutateSettings(ctx, accountID, func(rec *AccountSettingsRecord) { rec.EbsEncryptionByDefault = true }); err != nil {
		return nil, err
	}

	return &ec2.EnableEbsEncryptionByDefaultOutput{
		EbsEncryptionByDefault: aws.Bool(true),
	}, nil
}

// DisableEbsEncryptionByDefault disables EBS encryption by default for the account.
func (s *AccountSettingsServiceImpl) DisableEbsEncryptionByDefault(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, accountID string) (*ec2.DisableEbsEncryptionByDefaultOutput, error) {
	slog.InfoContext(ctx, "DisableEbsEncryptionByDefault called", "accountID", accountID)

	if err := s.mutateSettings(ctx, accountID, func(rec *AccountSettingsRecord) { rec.EbsEncryptionByDefault = false }); err != nil {
		return nil, err
	}

	return &ec2.DisableEbsEncryptionByDefaultOutput{
		EbsEncryptionByDefault: aws.Bool(false),
	}, nil
}

// GetEbsEncryptionByDefault gets the current EBS encryption by default setting.
func (s *AccountSettingsServiceImpl) GetEbsEncryptionByDefault(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, accountID string) (*ec2.GetEbsEncryptionByDefaultOutput, error) {
	slog.InfoContext(ctx, "GetEbsEncryptionByDefault called", "accountID", accountID)

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	return &ec2.GetEbsEncryptionByDefaultOutput{
		EbsEncryptionByDefault: aws.Bool(settings.EbsEncryptionByDefault),
	}, nil
}

// GetSerialConsoleAccessStatus gets the current serial console access status.
func (s *AccountSettingsServiceImpl) GetSerialConsoleAccessStatus(ctx context.Context, input *ec2.GetSerialConsoleAccessStatusInput, accountID string) (*ec2.GetSerialConsoleAccessStatusOutput, error) {
	slog.InfoContext(ctx, "GetSerialConsoleAccessStatus called", "accountID", accountID)

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	return &ec2.GetSerialConsoleAccessStatusOutput{
		SerialConsoleAccessEnabled: aws.Bool(settings.SerialConsoleAccess),
	}, nil
}

// EnableSerialConsoleAccess enables serial console access for the account.
func (s *AccountSettingsServiceImpl) EnableSerialConsoleAccess(ctx context.Context, input *ec2.EnableSerialConsoleAccessInput, accountID string) (*ec2.EnableSerialConsoleAccessOutput, error) {
	slog.InfoContext(ctx, "EnableSerialConsoleAccess called", "accountID", accountID)

	if err := s.mutateSettings(ctx, accountID, func(rec *AccountSettingsRecord) { rec.SerialConsoleAccess = true }); err != nil {
		return nil, err
	}

	return &ec2.EnableSerialConsoleAccessOutput{
		SerialConsoleAccessEnabled: aws.Bool(true),
	}, nil
}

// DisableSerialConsoleAccess disables serial console access for the account.
func (s *AccountSettingsServiceImpl) DisableSerialConsoleAccess(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, accountID string) (*ec2.DisableSerialConsoleAccessOutput, error) {
	slog.InfoContext(ctx, "DisableSerialConsoleAccess called", "accountID", accountID)

	if err := s.mutateSettings(ctx, accountID, func(rec *AccountSettingsRecord) { rec.SerialConsoleAccess = false }); err != nil {
		return nil, err
	}

	return &ec2.DisableSerialConsoleAccessOutput{
		SerialConsoleAccessEnabled: aws.Bool(false),
	}, nil
}
