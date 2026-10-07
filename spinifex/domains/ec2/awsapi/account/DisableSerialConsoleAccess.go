package account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateDisableSerialConsoleAccessInput(input *ec2.DisableSerialConsoleAccessInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func DisableSerialConsoleAccess(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, natsConn *nats.Conn, accountID string) (ec2.DisableSerialConsoleAccessOutput, error) {
	var output ec2.DisableSerialConsoleAccessOutput

	if err := ValidateDisableSerialConsoleAccessInput(input); err != nil {
		return output, err
	}

	svc := ec2account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.DisableSerialConsoleAccess(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
