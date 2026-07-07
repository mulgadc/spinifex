package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateRegisterTargetsInput(input *elbv2.RegisterTargetsInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Targets) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// RegisterTargets handles the ELBv2 RegisterTargets API call.
func RegisterTargets(ctx context.Context, input *elbv2.RegisterTargetsInput, natsConn *nats.Conn, accountID string) (elbv2.RegisterTargetsOutput, error) {
	var output elbv2.RegisterTargetsOutput

	if err := ValidateRegisterTargetsInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.RegisterTargets(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
