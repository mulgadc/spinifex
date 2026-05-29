package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateModifyTargetGroupAttributesInput(input *elbv2.ModifyTargetGroupAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Attributes) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyTargetGroupAttributes handles the ELBv2 ModifyTargetGroupAttributes API call.
func ModifyTargetGroupAttributes(input *elbv2.ModifyTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyTargetGroupAttributesOutput, error) {
	var output elbv2.ModifyTargetGroupAttributesOutput

	if err := ValidateModifyTargetGroupAttributesInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyTargetGroupAttributes(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
