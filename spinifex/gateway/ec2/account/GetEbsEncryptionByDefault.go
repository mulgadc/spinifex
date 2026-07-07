package gateway_ec2_account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	"github.com/nats-io/nats.go"
)

func ValidateGetEbsEncryptionByDefaultInput(input *ec2.GetEbsEncryptionByDefaultInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func GetEbsEncryptionByDefault(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, natsConn *nats.Conn, accountID string) (ec2.GetEbsEncryptionByDefaultOutput, error) {
	var output ec2.GetEbsEncryptionByDefaultOutput

	if err := ValidateGetEbsEncryptionByDefaultInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.GetEbsEncryptionByDefault(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
