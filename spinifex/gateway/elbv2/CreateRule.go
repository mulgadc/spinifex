package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateCreateRuleInput(input *elbv2.CreateRuleInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Priority == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Conditions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Actions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateRule handles the ELBv2 CreateRule API call.
func CreateRule(input *elbv2.CreateRuleInput, natsConn *nats.Conn, accountID string) (elbv2.CreateRuleOutput, error) {
	var output elbv2.CreateRuleOutput

	if err := ValidateCreateRuleInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.CreateRule(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
