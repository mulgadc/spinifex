package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeRulesInput(input *elbv2.DescribeRulesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	hasListener := input.ListenerArn != nil && *input.ListenerArn != ""
	hasRules := len(input.RuleArns) > 0
	if !hasListener && !hasRules {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeRules handles the ELBv2 DescribeRules API call.
func DescribeRules(input *elbv2.DescribeRulesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeRulesOutput, error) {
	var output elbv2.DescribeRulesOutput

	if err := ValidateDescribeRulesInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeRules(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
