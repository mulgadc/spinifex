package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateGetLBConfigInput(input *handlers_elbv2.GetLBConfigInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LBID == nil || *input.LBID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// GetLBConfig handles the ELBv2 GetLBConfig API call.
func GetLBConfig(ctx context.Context, input *handlers_elbv2.GetLBConfigInput, natsConn *nats.Conn, accountID string) (handlers_elbv2.GetLBConfigOutput, error) {
	var output handlers_elbv2.GetLBConfigOutput

	if err := ValidateGetLBConfigInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.GetLBConfig(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
