package account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateDisableEbsEncryptionByDefaultInput(input *ec2.DisableEbsEncryptionByDefaultInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func DisableEbsEncryptionByDefault(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, natsConn *nats.Conn, accountID string) (ec2.DisableEbsEncryptionByDefaultOutput, error) {
	var output ec2.DisableEbsEncryptionByDefaultOutput

	if err := ValidateDisableEbsEncryptionByDefaultInput(input); err != nil {
		return output, err
	}

	svc := ec2account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.DisableEbsEncryptionByDefault(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
