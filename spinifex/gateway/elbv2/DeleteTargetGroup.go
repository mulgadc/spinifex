package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteTargetGroupInput(input *elbv2.DeleteTargetGroupInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteTargetGroup handles the ELBv2 DeleteTargetGroup API call.
func DeleteTargetGroup(ctx context.Context, input *elbv2.DeleteTargetGroupInput, natsConn *nats.Conn, accountID string) (elbv2.DeleteTargetGroupOutput, error) {
	var output elbv2.DeleteTargetGroupOutput

	if err := ValidateDeleteTargetGroupInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteTargetGroup(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
