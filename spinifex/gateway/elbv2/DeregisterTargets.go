package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDeregisterTargetsInput(input *elbv2.DeregisterTargetsInput) error {
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

// DeregisterTargets handles the ELBv2 DeregisterTargets API call.
func DeregisterTargets(ctx context.Context, input *elbv2.DeregisterTargetsInput, natsConn *nats.Conn, accountID string) (elbv2.DeregisterTargetsOutput, error) {
	var output elbv2.DeregisterTargetsOutput

	if err := ValidateDeregisterTargetsInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DeregisterTargets(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
