package elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAWSError checks err carries code and renders AWS's message, either the
// one the call site attached or the table text the gateway falls back to.
func assertAWSError(t *testing.T, err error, code, message string) {
	t.Helper()
	gotCode, gotMsg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unresolved error: %v", err)
	assert.Equal(t, code, gotCode)
	if gotMsg == "" {
		gotMsg = awserrors.LookupErrorMessage("elasticloadbalancing", gotCode).Message
	}
	assert.Equal(t, message, gotMsg)
}

func TestErrorMessages_CreateListenerDuplicatePort(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-dupport")

	_, err := env.svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(env.lbArn),
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2DuplicateListener,
		"A listener already exists on this port for this load balancer '"+env.lbArn+"'")
}

func TestErrorMessages_ModifyListenerDuplicatePort(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-modport")

	l2, err := env.svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(env.lbArn),
		Port:            aws.Int64(8080),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn: l2.Listeners[0].ListenerArn,
		Port:        aws.Int64(80),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2DuplicateListener,
		"A listener already exists on this port for this load balancer '"+env.lbArn+"'")
}

func TestErrorMessages_ModifyListenerBadSslPolicy(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, listenerArn := attachedCertListener(t, svc, "lb-msg-ssl", "msg-ssl-lb")

	_, err := svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn: aws.String(listenerArn),
		SslPolicy:   aws.String("NoSuchPolicy"),
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2SSLPolicyNotFound, "SSL policy 'NoSuchPolicy' not found")
}

func createPathRule(t *testing.T, env ruleTestEnv, priority int64, path string) string {
	t.Helper()
	out, err := env.svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(priority),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{path})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)
	return *out.Rules[0].RuleArn
}

func TestErrorMessages_CreateRuleDuplicatePriority(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-crpri")
	createPathRule(t, env, 10, "/a")

	_, err := env.svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/b"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2PriorityInUse, "Priority '10' is currently in use")
}

func TestErrorMessages_SetRulePrioritiesConflict(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-spri")
	createPathRule(t, env, 10, "/a")
	r2 := createPathRule(t, env, 20, "/b")

	_, err := env.svc.SetRulePriorities(context.Background(), &elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{{RuleArn: aws.String(r2), Priority: aws.Int64(10)}},
	}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2PriorityInUse, "One or more priorities not found")
}

func TestErrorMessages_DescribeRulesDeleted(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-desc")
	ruleArn := createPathRule(t, env, 10, "/a")

	_, err := env.svc.DeleteRule(context.Background(), &elbv2.DeleteRuleInput{RuleArn: aws.String(ruleArn)}, testAccountID)
	require.NoError(t, err)
	_, err = env.svc.DescribeRules(context.Background(), &elbv2.DescribeRulesInput{RuleArns: []*string{aws.String(ruleArn)}}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2RuleNotFound, "One or more rules not found")

	_, err = env.svc.DeleteListener(context.Background(), &elbv2.DeleteListenerInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)
	_, err = env.svc.DescribeRules(context.Background(), &elbv2.DescribeRulesInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2ListenerNotFound, "One or more listeners not found")
}

func TestErrorMessages_DescribeLoadBalancersNotFoundTableText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "One or more load balancers not found",
		awserrors.LookupErrorMessage("elasticloadbalancing", awserrors.ErrorELBv2LoadBalancerNotFound).Message)
}

func TestErrorMessages_DeleteTargetGroupInUse(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "msg-tginuse")

	// Referenced by the listener's default action.
	_, err := env.svc.DeleteTargetGroup(context.Background(), &elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(env.tgArn)}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2TargetGroupInUse,
		"Target group '"+env.tgArn+"' is currently in use by a listener or a rule")

	// Referenced only by a rule.
	createPathRule(t, env, 10, "/a")
	_, err = env.svc.DeleteTargetGroup(context.Background(), &elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(env.tgAltArn)}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorELBv2TargetGroupInUse,
		"Target group '"+env.tgAltArn+"' is currently in use by a listener or a rule")
}
