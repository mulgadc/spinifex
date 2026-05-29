package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteLoadBalancerInput(input *elbv2.DeleteLoadBalancerInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteLoadBalancer handles the ELBv2 DeleteLoadBalancer API call.
func DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, natsConn *nats.Conn, accountID string) (elbv2.DeleteLoadBalancerOutput, error) {
	var output elbv2.DeleteLoadBalancerOutput

	if err := ValidateDeleteLoadBalancerInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteLoadBalancer(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
