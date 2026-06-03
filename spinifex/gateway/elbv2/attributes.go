package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// validateAttrArn rejects a nil or empty resource ARN, shared by every
// attribute Modify/Describe validator.
func validateAttrArn(arn *string) error {
	if arn == nil || *arn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// attributeOp validates an attribute request and dispatches it to its NATS
// service call, returning a zero output on any error. Shared by all four
// Modify/Describe attribute handlers.
func attributeOp[I, O any](input *I, validate func(*I) error, call func(*I) (*O, error)) (O, error) {
	var output O
	if err := validate(input); err != nil {
		return output, err
	}
	result, err := call(input)
	if err != nil {
		return output, err
	}
	return *result, nil
}

func ValidateModifyTargetGroupAttributesInput(input *elbv2.ModifyTargetGroupAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := validateAttrArn(input.TargetGroupArn); err != nil {
		return err
	}
	if len(input.Attributes) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func ValidateDescribeTargetGroupAttributesInput(input *elbv2.DescribeTargetGroupAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return validateAttrArn(input.TargetGroupArn)
}

func ValidateModifyLoadBalancerAttributesInput(input *elbv2.ModifyLoadBalancerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := validateAttrArn(input.LoadBalancerArn); err != nil {
		return err
	}
	if len(input.Attributes) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func ValidateDescribeLoadBalancerAttributesInput(input *elbv2.DescribeLoadBalancerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return validateAttrArn(input.LoadBalancerArn)
}

func ModifyTargetGroupAttributes(input *elbv2.ModifyTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyTargetGroupAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateModifyTargetGroupAttributesInput,
		func(i *elbv2.ModifyTargetGroupAttributesInput) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
			return svc.ModifyTargetGroupAttributes(i, accountID)
		})
}

func DescribeTargetGroupAttributes(input *elbv2.DescribeTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTargetGroupAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateDescribeTargetGroupAttributesInput,
		func(i *elbv2.DescribeTargetGroupAttributesInput) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
			return svc.DescribeTargetGroupAttributes(i, accountID)
		})
}

func ModifyLoadBalancerAttributes(input *elbv2.ModifyLoadBalancerAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyLoadBalancerAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateModifyLoadBalancerAttributesInput,
		func(i *elbv2.ModifyLoadBalancerAttributesInput) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
			return svc.ModifyLoadBalancerAttributes(i, accountID)
		})
}

func DescribeLoadBalancerAttributes(input *elbv2.DescribeLoadBalancerAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeLoadBalancerAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateDescribeLoadBalancerAttributesInput,
		func(i *elbv2.DescribeLoadBalancerAttributesInput) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
			return svc.DescribeLoadBalancerAttributes(i, accountID)
		})
}
