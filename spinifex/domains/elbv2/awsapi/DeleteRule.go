package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteRuleInput rejects a nil input with InvalidParameterValue and a missing RuleArn
// with MissingParameter.
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

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteRule(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
