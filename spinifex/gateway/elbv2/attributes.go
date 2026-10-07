package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// validateAttrArn rejects a nil or empty resource ARN.
func validateAttrArn(arn *string) error {
	if arn == nil || *arn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// attributeOp validates input then dispatches to the NATS service call, shared
// by all four Modify/Describe attribute handlers.
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

// ValidateModifyTargetGroupAttributesInput rejects a nil input with InvalidParameterValue and a
// missing TargetGroupArn or empty Attributes with MissingParameter.
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

// ValidateDescribeTargetGroupAttributesInput rejects a nil input with InvalidParameterValue and a
// missing TargetGroupArn with MissingParameter.
func ValidateDescribeTargetGroupAttributesInput(input *elbv2.DescribeTargetGroupAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return validateAttrArn(input.TargetGroupArn)
}

// ValidateModifyLoadBalancerAttributesInput rejects a nil input with InvalidParameterValue and a
// missing LoadBalancerArn or empty Attributes with MissingParameter.
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

// ValidateDescribeLoadBalancerAttributesInput rejects a nil input with InvalidParameterValue and
// a missing LoadBalancerArn with MissingParameter.
func ValidateDescribeLoadBalancerAttributesInput(input *elbv2.DescribeLoadBalancerAttributesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return validateAttrArn(input.LoadBalancerArn)
}

// ModifyTargetGroupAttributes implements the ELBv2 ModifyTargetGroupAttributes action, validating
// the input and forwarding it to the NATS ELBv2 service.
func ModifyTargetGroupAttributes(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyTargetGroupAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateModifyTargetGroupAttributesInput,
		func(i *elbv2.ModifyTargetGroupAttributesInput) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
			return svc.ModifyTargetGroupAttributes(ctx, i, accountID)
		})
}

// DescribeTargetGroupAttributes implements the ELBv2 DescribeTargetGroupAttributes action,
// validating the input and forwarding it to the NATS ELBv2 service.
func DescribeTargetGroupAttributes(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTargetGroupAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateDescribeTargetGroupAttributesInput,
		func(i *elbv2.DescribeTargetGroupAttributesInput) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
			return svc.DescribeTargetGroupAttributes(ctx, i, accountID)
		})
}

// ModifyLoadBalancerAttributes implements the ELBv2 ModifyLoadBalancerAttributes action,
// validating the input and forwarding it to the NATS ELBv2 service.
func ModifyLoadBalancerAttributes(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.ModifyLoadBalancerAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateModifyLoadBalancerAttributesInput,
		func(i *elbv2.ModifyLoadBalancerAttributesInput) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
			return svc.ModifyLoadBalancerAttributes(ctx, i, accountID)
		})
}

// DescribeLoadBalancerAttributes implements the ELBv2 DescribeLoadBalancerAttributes action,
// validating the input and forwarding it to the NATS ELBv2 service.
func DescribeLoadBalancerAttributes(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeLoadBalancerAttributesOutput, error) {
	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	return attributeOp(input, ValidateDescribeLoadBalancerAttributesInput,
		func(i *elbv2.DescribeLoadBalancerAttributesInput) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
			return svc.DescribeLoadBalancerAttributes(ctx, i, accountID)
		})
}
