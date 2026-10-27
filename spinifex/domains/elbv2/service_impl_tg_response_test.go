package elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

func describeTG(t *testing.T, svc *ELBv2ServiceImpl, arn string) *elbv2.TargetGroup {
	t.Helper()
	out, err := svc.DescribeTargetGroups(context.Background(), &elbv2.DescribeTargetGroupsInput{
		TargetGroupArns: []*string{aws.String(arn)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TargetGroups, 1)
	return out.TargetGroups[0]
}

// Terraform treats ip_address_type as force-new, so an absent value on read
// replaces the target group on every plan.
func TestTargetGroup_IpAddressTypeDefaultsToIPv4(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("ipat-tg")}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.TargetGroups[0].IpAddressType)
	assert.Equal(t, "ipv4", *out.TargetGroups[0].IpAddressType)
	assert.Empty(t, out.TargetGroups[0].LoadBalancerArns)

	tg := describeTG(t, svc, *out.TargetGroups[0].TargetGroupArn)
	require.NotNil(t, tg.IpAddressType)
	assert.Equal(t, "ipv4", *tg.IpAddressType)
}

func TestCreateTargetGroup_IpAddressType(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name:          aws.String("ipat-v4"),
		IpAddressType: aws.String("ipv4"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ipv4", *out.TargetGroups[0].IpAddressType)

	_, err = svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name:          aws.String("ipat-v6"),
		IpAddressType: aws.String("ipv6"),
	}, testAccountID)
	assert.ErrorContains(t, err, awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestTargetGroup_LoadBalancerArns(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "tg-lbarns")

	// The alt TG is not referenced until a rule forwards to it.
	assert.Empty(t, describeTG(t, env.svc, env.tgAltArn).LoadBalancerArns)
	assert.Equal(t, []*string{aws.String(env.lbArn)}, describeTG(t, env.svc, env.tgArn).LoadBalancerArns)

	_, err := env.svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/api/*"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = env.svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(20),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/alt/*"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	// A TG reached by both a listener and a rule on the same LB lists it once.
	assert.Equal(t, []*string{aws.String(env.lbArn)}, describeTG(t, env.svc, env.tgArn).LoadBalancerArns)
	assert.Equal(t, []*string{aws.String(env.lbArn)}, describeTG(t, env.svc, env.tgAltArn).LoadBalancerArns)

	mod, err := env.svc.ModifyTargetGroup(context.Background(), &elbv2.ModifyTargetGroupInput{
		TargetGroupArn:  aws.String(env.tgAltArn),
		HealthCheckPath: aws.String("/healthz"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []*string{aws.String(env.lbArn)}, mod.TargetGroups[0].LoadBalancerArns)

	_, err = env.svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(env.lbArn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, describeTG(t, env.svc, env.tgArn).LoadBalancerArns)
}
