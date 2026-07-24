package handlers_ec2_account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketAccountSettings        = "spinifex-ec2-account-settings"
	KVBucketAccountSettingsVersion = 1
	KeyEbsEncryptionDefault        = "ebs-encryption-default"
	KeySerialConsoleAccess         = "serial-console-access"
)

// AccountSettingsRecord represents stored account settings.
type AccountSettingsRecord struct {
	EbsEncryptionByDefault bool `json:"ebs_encryption_by_default"`
	SerialConsoleAccess    bool `json:"serial_console_access"`
}

// AccountSettingsServiceImpl implements account settings operations with NATS JetStream persistence.
type AccountSettingsServiceImpl struct {
	config     *config.Config
	js         jetstream.JetStream
	settingsKV jetstream.KeyValue
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
		return utils.GlobalAccountID
	}
	return accountID
}

// getSettings retrieves current account settings.
func (s *AccountSettingsServiceImpl) getSettings(ctx context.Context, accountID string) (*AccountSettingsRecord, error) {
	entry, err := s.settingsKV.Get(ctx, settingsKey(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return &AccountSettingsRecord{
				EbsEncryptionByDefault: false,
				SerialConsoleAccess:    false,
			}, nil
		}
		return nil, fmt.Errorf("failed to get account settings: %w", err)
	}

	var settings AccountSettingsRecord
	if err := json.Unmarshal(entry.Value(), &settings); err != nil {
		return nil, err
	}

	return &settings, nil
}

// saveSettings saves current account settings.
func (s *AccountSettingsServiceImpl) saveSettings(ctx context.Context, settings *AccountSettingsRecord, accountID string) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}

	_, err = s.settingsKV.Put(ctx, settingsKey(accountID), data)
	return err
}

// EnableEbsEncryptionByDefault enables EBS encryption by default for the account.
func (s *AccountSettingsServiceImpl) EnableEbsEncryptionByDefault(ctx context.Context, input *ec2.EnableEbsEncryptionByDefaultInput, accountID string) (*ec2.EnableEbsEncryptionByDefaultOutput, error) {
	slog.InfoContext(ctx, "EnableEbsEncryptionByDefault called", "accountID", accountID)

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	settings.EbsEncryptionByDefault = true

	if err := s.saveSettings(ctx, settings, accountID); err != nil {
		return nil, err
	}

	return &ec2.EnableEbsEncryptionByDefaultOutput{
		EbsEncryptionByDefault: aws.Bool(true),
	}, nil
}

// DisableEbsEncryptionByDefault disables EBS encryption by default for the account.
func (s *AccountSettingsServiceImpl) DisableEbsEncryptionByDefault(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, accountID string) (*ec2.DisableEbsEncryptionByDefaultOutput, error) {
	slog.InfoContext(ctx, "DisableEbsEncryptionByDefault called", "accountID", accountID)

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	settings.EbsEncryptionByDefault = false

	if err := s.saveSettings(ctx, settings, accountID); err != nil {
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

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	settings.SerialConsoleAccess = true

	if err := s.saveSettings(ctx, settings, accountID); err != nil {
		return nil, err
	}

	return &ec2.EnableSerialConsoleAccessOutput{
		SerialConsoleAccessEnabled: aws.Bool(true),
	}, nil
}

// DisableSerialConsoleAccess disables serial console access for the account.
func (s *AccountSettingsServiceImpl) DisableSerialConsoleAccess(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, accountID string) (*ec2.DisableSerialConsoleAccessOutput, error) {
	slog.InfoContext(ctx, "DisableSerialConsoleAccess called", "accountID", accountID)

	settings, err := s.getSettings(ctx, accountID)
	if err != nil {
		return nil, err
	}

	settings.SerialConsoleAccess = false

	if err := s.saveSettings(ctx, settings, accountID); err != nil {
		return nil, err
	}

	return &ec2.DisableSerialConsoleAccessOutput{
		SerialConsoleAccessEnabled: aws.Bool(false),
	}, nil
}
