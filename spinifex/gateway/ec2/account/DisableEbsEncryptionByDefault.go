package gateway_ec2_account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	"github.com/nats-io/nats.go"
)

// ValidateDisableEbsEncryptionByDefaultInput rejects a nil input with InvalidParameterValue; the
// action takes no parameters.
func ValidateDisableEbsEncryptionByDefaultInput(input *ec2.DisableEbsEncryptionByDefaultInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// DisableEbsEncryptionByDefault implements the EC2 DisableEbsEncryptionByDefault action, turning
// off default encryption of new EBS volumes for accountID via the NATS account settings service.
func DisableEbsEncryptionByDefault(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, natsConn *nats.Conn, accountID string) (ec2.DisableEbsEncryptionByDefaultOutput, error) {
	var output ec2.DisableEbsEncryptionByDefaultOutput

	if err := ValidateDisableEbsEncryptionByDefaultInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.DisableEbsEncryptionByDefault(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
