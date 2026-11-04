package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDescribeSSLPoliciesInput rejects a nil input with InvalidParameterValue and a
// LoadBalancerType outside the model with ValidationError.
func ValidateDescribeSSLPoliciesInput(input *elbv2.DescribeSSLPoliciesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return checkEnum(input.LoadBalancerType, "loadBalancerType", loadBalancerTypes)
}

// DescribeSSLPolicies handles the ELBv2 DescribeSSLPolicies API call. It serves
// the fixed catalog of supported security policies.
func DescribeSSLPolicies(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeSSLPoliciesOutput, error) {
	var output elbv2.DescribeSSLPoliciesOutput

	if err := ValidateDescribeSSLPoliciesInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeSSLPolicies(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
