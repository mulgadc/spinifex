package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCreateListenerInput rejects a nil input with InvalidParameterValue and a missing
// LoadBalancerArn or empty DefaultActions with MissingParameter.
func ValidateCreateListenerInput(input *elbv2.CreateListenerInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.DefaultActions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateListener handles the ELBv2 CreateListener API call.
func CreateListener(ctx context.Context, input *elbv2.CreateListenerInput, natsConn *nats.Conn, accountID string) (elbv2.CreateListenerOutput, error) {
	var output elbv2.CreateListenerOutput

	if err := ValidateCreateListenerInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.CreateListener(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
