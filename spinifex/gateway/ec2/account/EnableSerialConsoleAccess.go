package gateway_ec2_account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateEnableSerialConsoleAccessInput(input *ec2.EnableSerialConsoleAccessInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func EnableSerialConsoleAccess(ctx context.Context, input *ec2.EnableSerialConsoleAccessInput, natsConn *nats.Conn, accountID string) (ec2.EnableSerialConsoleAccessOutput, error) {
	var output ec2.EnableSerialConsoleAccessOutput

	if err := ValidateEnableSerialConsoleAccessInput(input); err != nil {
		return output, err
	}

	svc := ec2account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.EnableSerialConsoleAccess(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
