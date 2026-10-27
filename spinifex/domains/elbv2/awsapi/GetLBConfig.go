package awsapi

import (
	"context"
	"errors"

	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateGetLBConfigInput rejects a nil input with InvalidParameterValue and a missing LBID with
// MissingParameter.
func ValidateGetLBConfigInput(input *elbv2domain.GetLBConfigInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LBID == nil || *input.LBID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// GetLBConfig handles the ELBv2 GetLBConfig API call.
func GetLBConfig(ctx context.Context, input *elbv2domain.GetLBConfigInput, natsConn *nats.Conn, accountID string) (elbv2domain.GetLBConfigOutput, error) {
	var output elbv2domain.GetLBConfigOutput

	if err := ValidateGetLBConfigInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.GetLBConfig(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
