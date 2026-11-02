package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteLoadBalancerInput rejects a nil input with InvalidParameterValue and a missing
// LoadBalancerArn with MissingParameter.
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
func DeleteLoadBalancer(ctx context.Context, input *elbv2.DeleteLoadBalancerInput, natsConn *nats.Conn, accountID string) (elbv2.DeleteLoadBalancerOutput, error) {
	var output elbv2.DeleteLoadBalancerOutput

	if err := ValidateDeleteLoadBalancerInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.DeleteLoadBalancer(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
