package account

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateGetSerialConsoleAccessStatusInput rejects a nil input with InvalidParameterValue; the
// action takes no parameters.
func ValidateGetSerialConsoleAccessStatusInput(input *ec2.GetSerialConsoleAccessStatusInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// GetSerialConsoleAccessStatus implements the EC2 GetSerialConsoleAccessStatus action, reporting
// whether serial console access is enabled for accountID.
func GetSerialConsoleAccessStatus(ctx context.Context, input *ec2.GetSerialConsoleAccessStatusInput, natsConn *nats.Conn, accountID string) (ec2.GetSerialConsoleAccessStatusOutput, error) {
	var output ec2.GetSerialConsoleAccessStatusOutput

	if err := ValidateGetSerialConsoleAccessStatusInput(input); err != nil {
		return output, err
	}

	svc := ec2account.NewNATSAccountSettingsService(natsConn)
	result, err := svc.GetSerialConsoleAccessStatus(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
