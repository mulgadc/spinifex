package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateModifyTargetGroupInput(input *elbv2.ModifyTargetGroupInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func ModifyTargetGroup(input *elbv2.ModifyTargetGroupInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyTargetGroupOutput, error) {
	var output elbv2.ModifyTargetGroupOutput

	if err := ValidateModifyTargetGroupInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyTargetGroup(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
