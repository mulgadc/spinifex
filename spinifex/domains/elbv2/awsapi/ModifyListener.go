package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateModifyListenerInput rejects a nil input with InvalidParameterValue and a missing
// ListenerArn with MissingParameter.
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

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyListener(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
