package handlers_ec2_account

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ AccountSettingsService = (*NATSAccountSettingsService)(nil)

// NATSAccountSettingsService implements AccountSettingsService via NATS messaging
type NATSAccountSettingsService struct {
	natsConn *nats.Conn
}

// NewNATSAccountSettingsService creates a new NATS-based account settings service
func NewNATSAccountSettingsService(natsConn *nats.Conn) AccountSettingsService {
	return &NATSAccountSettingsService{natsConn: natsConn}
}

func (s *NATSAccountSettingsService) EnableEbsEncryptionByDefault(ctx context.Context, input *ec2.EnableEbsEncryptionByDefaultInput, accountID string) (*ec2.EnableEbsEncryptionByDefaultOutput, error) {
	return utils.NATSRequest[ec2.EnableEbsEncryptionByDefaultOutput](ctx, s.natsConn, "ec2.EnableEbsEncryptionByDefault", input, 30*time.Second, accountID)
}

func (s *NATSAccountSettingsService) DisableEbsEncryptionByDefault(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, accountID string) (*ec2.DisableEbsEncryptionByDefaultOutput, error) {
	return utils.NATSRequest[ec2.DisableEbsEncryptionByDefaultOutput](ctx, s.natsConn, "ec2.DisableEbsEncryptionByDefault", input, 30*time.Second, accountID)
}

func (s *NATSAccountSettingsService) GetEbsEncryptionByDefault(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, accountID string) (*ec2.GetEbsEncryptionByDefaultOutput, error) {
	return utils.NATSRequest[ec2.GetEbsEncryptionByDefaultOutput](ctx, s.natsConn, "ec2.GetEbsEncryptionByDefault", input, 30*time.Second, accountID)
}

func (s *NATSAccountSettingsService) GetSerialConsoleAccessStatus(ctx context.Context, input *ec2.GetSerialConsoleAccessStatusInput, accountID string) (*ec2.GetSerialConsoleAccessStatusOutput, error) {
	return utils.NATSRequest[ec2.GetSerialConsoleAccessStatusOutput](ctx, s.natsConn, "ec2.GetSerialConsoleAccessStatus", input, 30*time.Second, accountID)
}

func (s *NATSAccountSettingsService) EnableSerialConsoleAccess(ctx context.Context, input *ec2.EnableSerialConsoleAccessInput, accountID string) (*ec2.EnableSerialConsoleAccessOutput, error) {
	return utils.NATSRequest[ec2.EnableSerialConsoleAccessOutput](ctx, s.natsConn, "ec2.EnableSerialConsoleAccess", input, 30*time.Second, accountID)
}

func (s *NATSAccountSettingsService) DisableSerialConsoleAccess(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, accountID string) (*ec2.DisableSerialConsoleAccessOutput, error) {
	return utils.NATSRequest[ec2.DisableSerialConsoleAccessOutput](ctx, s.natsConn, "ec2.DisableSerialConsoleAccess", input, 30*time.Second, accountID)
}
