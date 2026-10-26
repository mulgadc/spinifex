package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateModifyTargetGroupInput rejects a nil input with InvalidParameterValue and a missing
// TargetGroupArn with MissingParameter.
func ValidateModifyTargetGroupInput(input *elbv2.ModifyTargetGroupInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyTargetGroup implements the ELBv2 ModifyTargetGroup action, updating health check settings
// via the NATS ELBv2 service scoped to accountID.
func ModifyTargetGroup(ctx context.Context, input *elbv2.ModifyTargetGroupInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyTargetGroupOutput, error) {
	var output elbv2.ModifyTargetGroupOutput

	if err := ValidateModifyTargetGroupInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.ModifyTargetGroup(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
