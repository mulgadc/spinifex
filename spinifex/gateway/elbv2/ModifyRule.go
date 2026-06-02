package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateModifyRuleInput(input *elbv2.ModifyRuleInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RuleArn == nil || *input.RuleArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Conditions) == 0 && len(input.Actions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyRule handles the ELBv2 ModifyRule API call.
func ModifyRule(input *elbv2.ModifyRuleInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyRuleOutput, error) {
	var output elbv2.ModifyRuleOutput

	if err := ValidateModifyRuleInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyRule(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
