package gateway_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

// Input-validation tests; no NATS connection needed.

func TestCreateLoadBalancer_NilInput(t *testing.T) {
	_, err := CreateLoadBalancer(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateLoadBalancer_MissingName(t *testing.T) {
	_, err := CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateLoadBalancer_EmptyName(t *testing.T) {
	_, err := CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteLoadBalancer_NilInput(t *testing.T) {
	_, err := DeleteLoadBalancer(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteLoadBalancer_MissingArn(t *testing.T) {
	_, err := DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeLoadBalancers_NilInput(t *testing.T) {
	_, err := DescribeLoadBalancers(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateTargetGroup_NilInput(t *testing.T) {
	_, err := CreateTargetGroup(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateTargetGroup_MissingName(t *testing.T) {
	_, err := CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteTargetGroup_NilInput(t *testing.T) {
	_, err := DeleteTargetGroup(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteTargetGroup_MissingArn(t *testing.T) {
	_, err := DeleteTargetGroup(context.Background(), &elbv2.DeleteTargetGroupInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeTargetGroups_NilInput(t *testing.T) {
	_, err := DescribeTargetGroups(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestRegisterTargets_NilInput(t *testing.T) {
	_, err := RegisterTargets(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestRegisterTargets_MissingArn(t *testing.T) {
	_, err := RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestRegisterTargets_MissingTargets(t *testing.T) {
	_, err := RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/my-tg/abc"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeregisterTargets_NilInput(t *testing.T) {
	_, err := DeregisterTargets(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeregisterTargets_MissingArn(t *testing.T) {
	_, err := DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeregisterTargets_MissingTargets(t *testing.T) {
	_, err := DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/my-tg/abc"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeTargetHealth_NilInput(t *testing.T) {
	_, err := DescribeTargetHealth(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeTargetHealth_MissingArn(t *testing.T) {
	_, err := DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateListener_NilInput(t *testing.T) {
	_, err := CreateListener(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateListener_MissingLBArn(t *testing.T) {
	_, err := CreateListener(context.Background(), &elbv2.CreateListenerInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateListener_MissingActions(t *testing.T) {
	_, err := CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/abc"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteListener_NilInput(t *testing.T) {
	_, err := DeleteListener(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteListener_MissingArn(t *testing.T) {
	_, err := DeleteListener(context.Background(), &elbv2.DeleteListenerInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyListener_NilInput(t *testing.T) {
	_, err := ModifyListener(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyListener_MissingArn(t *testing.T) {
	_, err := ModifyListener(context.Background(), &elbv2.ModifyListenerInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListeners_NilInput(t *testing.T) {
	_, err := DescribeListeners(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyTargetGroupAttributes_NilInput(t *testing.T) {
	_, err := ModifyTargetGroupAttributes(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyTargetGroupAttributes_MissingArn(t *testing.T) {
	_, err := ModifyTargetGroupAttributes(context.Background(), &elbv2.ModifyTargetGroupAttributesInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyTargetGroupAttributes_EmptyArn(t *testing.T) {
	_, err := ModifyTargetGroupAttributes(context.Background(), &elbv2.ModifyTargetGroupAttributesInput{
		TargetGroupArn: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyTargetGroupAttributes_MissingAttributes(t *testing.T) {
	_, err := ModifyTargetGroupAttributes(context.Background(), &elbv2.ModifyTargetGroupAttributesInput{
		TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/my-tg/abc"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeTargetGroupAttributes_NilInput(t *testing.T) {
	_, err := DescribeTargetGroupAttributes(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeTargetGroupAttributes_MissingArn(t *testing.T) {
	_, err := DescribeTargetGroupAttributes(context.Background(), &elbv2.DescribeTargetGroupAttributesInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeTargetGroupAttributes_EmptyArn(t *testing.T) {
	_, err := DescribeTargetGroupAttributes(context.Background(), &elbv2.DescribeTargetGroupAttributesInput{
		TargetGroupArn: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyLoadBalancerAttributes_NilInput(t *testing.T) {
	_, err := ModifyLoadBalancerAttributes(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyLoadBalancerAttributes_MissingArn(t *testing.T) {
	_, err := ModifyLoadBalancerAttributes(context.Background(), &elbv2.ModifyLoadBalancerAttributesInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyLoadBalancerAttributes_EmptyArn(t *testing.T) {
	_, err := ModifyLoadBalancerAttributes(context.Background(), &elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyLoadBalancerAttributes_MissingAttributes(t *testing.T) {
	_, err := ModifyLoadBalancerAttributes(context.Background(), &elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/abc"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeLoadBalancerAttributes_NilInput(t *testing.T) {
	_, err := DescribeLoadBalancerAttributes(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeLoadBalancerAttributes_MissingArn(t *testing.T) {
	_, err := DescribeLoadBalancerAttributes(context.Background(), &elbv2.DescribeLoadBalancerAttributesInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeLoadBalancerAttributes_EmptyArn(t *testing.T) {
	_, err := DescribeLoadBalancerAttributes(context.Background(), &elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListenerAttributes_NilInput(t *testing.T) {
	_, err := DescribeListenerAttributes(nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeListenerAttributes_MissingArn(t *testing.T) {
	_, err := DescribeListenerAttributes(&DescribeListenerAttributesInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListenerAttributes_EmptyArn(t *testing.T) {
	_, err := DescribeListenerAttributes(&DescribeListenerAttributesInput{
		ListenerArn: aws.String(""),
	}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeListenerAttributes_OK(t *testing.T) {
	out, err := DescribeListenerAttributes(&DescribeListenerAttributesInput{
		ListenerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/lb/abc/def"),
	}, "123456789012")
	assert.NoError(t, err)
	assert.Empty(t, out.Attributes)
}

func TestModifyListenerAttributes_NilInput(t *testing.T) {
	_, err := ModifyListenerAttributes(nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyListenerAttributes_MissingArn(t *testing.T) {
	_, err := ModifyListenerAttributes(&ModifyListenerAttributesInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyListenerAttributes_EchoesAttributes(t *testing.T) {
	attrs := []ListenerAttribute{{Key: aws.String("k"), Value: aws.String("v")}}
	out, err := ModifyListenerAttributes(&ModifyListenerAttributesInput{
		ListenerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/lb/abc/def"),
		Attributes:  attrs,
	}, "123456789012")
	assert.NoError(t, err)
	assert.Equal(t, attrs, out.Attributes)
}

func TestModifyTargetGroup_NilInput(t *testing.T) {
	_, err := ModifyTargetGroup(context.Background(), nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyTargetGroup_MissingArn(t *testing.T) {
	_, err := ModifyTargetGroup(context.Background(), &elbv2.ModifyTargetGroupInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyTargetGroup_EmptyArn(t *testing.T) {
	_, err := ModifyTargetGroup(context.Background(), &elbv2.ModifyTargetGroupInput{
		TargetGroupArn: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}
