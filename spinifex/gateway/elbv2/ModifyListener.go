package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateModifyListenerInput(input *elbv2.ModifyListenerInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyListener handles the ELBv2 ModifyListener API call.
func ModifyListener(ctx context.Context, input *elbv2.ModifyListenerInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyListenerOutput, error) {
	var output elbv2.ModifyListenerOutput

	if err := ValidateModifyListenerInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyListener(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
