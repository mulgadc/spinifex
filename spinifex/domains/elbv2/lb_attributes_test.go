package elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lbAttrMap(attrs []*elbv2.LoadBalancerAttribute) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[aws.StringValue(a.Key)] = aws.StringValue(a.Value)
	}
	return m
}

func TestModifyLoadBalancerAttributes_ReturnsFullSet(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	putFwdALB(t, svc)

	out, err := svc.ModifyLoadBalancerAttributes(t.Context(), &elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(fwdALBArn),
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String("idle_timeout.timeout_seconds"), Value: aws.String("120")},
		},
	}, testAccountID)
	require.NoError(t, err)

	want := DefaultLoadBalancerAttributes(LoadBalancerTypeApplication)
	want["idle_timeout.timeout_seconds"] = "120"
	assert.Len(t, out.Attributes, 22)
	assert.Equal(t, want, lbAttrMap(out.Attributes))
}

func TestModifyLoadBalancerAttributes_UnknownKeyMessage(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	putFwdALB(t, svc)

	err := modifyLBAttr(t, svc, "no.such.attribute", "true")
	require.Error(t, err)
	code, msg, _ := awserrors.ResolveErrorDetail(err)
	assert.Equal(t, awserrors.ErrorValidationError, code)
	assert.Equal(t, "Load balancer attribute key 'no.such.attribute' is not recognized", msg)
}

func TestModifyLoadBalancerAttributes_IdleTimeoutLowerBound(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-5", ""} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			svc := setupTestService(t)
			putFwdALB(t, svc)
			require.NoError(t, modifyLBAttr(t, svc, "idle_timeout.timeout_seconds", "120"))
			before := getLB(t, svc, "alb-fwd")

			err := modifyLBAttr(t, svc, "idle_timeout.timeout_seconds", value)
			require.Error(t, err)
			code, msg, _ := awserrors.ResolveErrorDetail(err)
			assert.Equal(t, awserrors.ErrorValidationError, code)
			assert.Equal(t, "The value of 'idle_timeout.timeout_seconds' cannot be less than 1 characters", msg)
			assert.Equal(t, before, getLB(t, svc, "alb-fwd"), "a rejected value is not stored")
		})
	}

	svc := setupTestService(t)
	putFwdALB(t, svc)
	require.NoError(t, modifyLBAttr(t, svc, "idle_timeout.timeout_seconds", "1"))
	assert.Equal(t, "1", getLB(t, svc, "alb-fwd").Attributes["idle_timeout.timeout_seconds"])
}
