package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCreateLoadBalancerInput rejects a nil input with InvalidParameterValue and a missing
// Name with MissingParameter.
func ValidateCreateLoadBalancerInput(input *elbv2.CreateLoadBalancerInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.Name == nil || *input.Name == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateLoadBalancer handles the ELBv2 CreateLoadBalancer API call.
func CreateLoadBalancer(ctx context.Context, input *elbv2.CreateLoadBalancerInput, natsConn *nats.Conn, accountID string) (elbv2.CreateLoadBalancerOutput, error) {
	var output elbv2.CreateLoadBalancerOutput

	if err := ValidateCreateLoadBalancerInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.CreateLoadBalancer(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
