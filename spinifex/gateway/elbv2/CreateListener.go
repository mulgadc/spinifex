package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

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
func CreateListener(input *elbv2.CreateListenerInput, natsConn *nats.Conn, accountID string) (elbv2.CreateListenerOutput, error) {
	var output elbv2.CreateListenerOutput

	if err := ValidateCreateListenerInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.CreateListener(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
