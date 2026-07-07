package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteListenerInput(input *elbv2.DeleteListenerInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteListener handles the ELBv2 DeleteListener API call.
func DeleteListener(ctx context.Context, input *elbv2.DeleteListenerInput, natsConn *nats.Conn, accountID string) (elbv2.DeleteListenerOutput, error) {
	var output elbv2.DeleteListenerOutput

	if err := ValidateDeleteListenerInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteListener(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
