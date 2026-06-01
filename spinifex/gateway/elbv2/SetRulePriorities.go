package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateSetRulePrioritiesInput(input *elbv2.SetRulePrioritiesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.RulePriorities) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// SetRulePriorities handles the ELBv2 SetRulePriorities API call.
func SetRulePriorities(input *elbv2.SetRulePrioritiesInput, natsConn *nats.Conn, accountID string) (elbv2.SetRulePrioritiesOutput, error) {
	var output elbv2.SetRulePrioritiesOutput

	if err := ValidateSetRulePrioritiesInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.SetRulePriorities(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
