package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteRuleInput(input *elbv2.DeleteRuleInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RuleArn == nil || *input.RuleArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteRule handles the ELBv2 DeleteRule API call.
func DeleteRule(ctx context.Context, input *elbv2.DeleteRuleInput, natsConn *nats.Conn, accountID string) (elbv2.DeleteRuleOutput, error) {
	var output elbv2.DeleteRuleOutput

	if err := ValidateDeleteRuleInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteRule(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
