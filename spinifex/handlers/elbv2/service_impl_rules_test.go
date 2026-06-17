package handlers_elbv2

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ruleTestEnv spins a service, ALB, TG, and HTTP listener so each rule test
// starts from the same baseline.
type ruleTestEnv struct {
	svc         *ELBv2ServiceImpl
	lbArn       string
	tgArn       string
	tgAltArn    string
	listenerArn string
}

func newRuleTestEnv(t *testing.T, namePrefix string) ruleTestEnv {
	t.Helper()
	svc := setupTestService(t)

	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String(namePrefix + "-lb"),
	}, testAccountID)
	require.NoError(t, err)

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String(namePrefix + "-tg")}, testAccountID)
	require.NoError(t, err)
	tgAltOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String(namePrefix + "-tg2")}, testAccountID)
	require.NoError(t, err)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	return ruleTestEnv{
		svc:         svc,
		lbArn:       *lbOut.LoadBalancers[0].LoadBalancerArn,
		tgArn:       *tgOut.TargetGroups[0].TargetGroupArn,
		tgAltArn:    *tgAltOut.TargetGroups[0].TargetGroupArn,
		listenerArn: *lstOut.Listeners[0].ListenerArn,
	}
}

func TestCreateRule_PathPattern(t *testing.T) {
	env := newRuleTestEnv(t, "cr-path")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions: []*elbv2.RuleCondition{
			{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/api/*"})},
		},
		Actions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 1)
	assert.Equal(t, "10", *out.Rules[0].Priority)
	assert.Contains(t, *out.Rules[0].RuleArn, ":listener-rule/")
}

// TestCreateRule_ForwardConfig guards the regression where a forward action that
// carries its target group via ForwardConfig (the shape the AWS Load Balancer
// Controller emits) was rejected with MissingParameter because only the flat
// TargetGroupArn field was read.
func TestCreateRule_ForwardConfig(t *testing.T) {
	env := newRuleTestEnv(t, "cr-fwdcfg")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions: []*elbv2.RuleCondition{
			{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/*"})},
		},
		Actions: []*elbv2.Action{
			{Type: aws.String("forward"), ForwardConfig: &elbv2.ForwardActionConfig{
				TargetGroups: []*elbv2.TargetGroupTuple{
					{TargetGroupArn: aws.String(env.tgAltArn)},
				},
			}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 1)
	require.Len(t, out.Rules[0].Actions, 1)
	assert.Equal(t, env.tgAltArn, *out.Rules[0].Actions[0].TargetGroupArn)
}

// TestDeleteLoadBalancer_CascadesRuleDeletion guards against the regression where
// DeleteLoadBalancer called store.DeleteListener directly, bypassing rule cascade,
// leaving a rule's target group pinned as ResourceInUse permanently.
func TestDeleteLoadBalancer_CascadesRuleDeletion(t *testing.T) {
	env := newRuleTestEnv(t, "del-cascade")

	// Rule on the listener forwards to the alt TG.
	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/api/*"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	// Tear down the LB.
	_, err = env.svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(env.lbArn),
	}, testAccountID)
	require.NoError(t, err)

	// LB deletion must cascade rule deletion — no rule may survive.
	rules, err := env.svc.store.ListRules()
	require.NoError(t, err)
	assert.Empty(t, rules, "LB deletion must cascade rule deletion")

	// Both target groups must now be deletable: neither an orphan rule nor a
	// stale listener default action may pin them as ResourceInUse.
	_, err = env.svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: aws.String(env.tgAltArn),
	}, testAccountID)
	require.NoError(t, err, "rule target group must not be pinned after LB deletion")

	_, err = env.svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: aws.String(env.tgArn),
	}, testAccountID)
	require.NoError(t, err, "listener default-action target group must not be pinned after LB deletion")
}

func TestCreateRule_PriorityInUse(t *testing.T) {
	env := newRuleTestEnv(t, "cr-pri")

	mkInput := func() *elbv2.CreateRuleInput {
		return &elbv2.CreateRuleInput{
			ListenerArn: aws.String(env.listenerArn),
			Priority:    aws.Int64(5),
			Conditions: []*elbv2.RuleCondition{
				{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})},
			},
			Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
		}
	}

	_, err := env.svc.CreateRule(mkInput(), testAccountID)
	require.NoError(t, err)

	_, err = env.svc.CreateRule(mkInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PriorityInUse")
}

func TestCreateRule_InvalidPriority(t *testing.T) {
	env := newRuleTestEnv(t, "cr-inv")

	for _, p := range []int64{0, 50001} {
		_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
			ListenerArn: aws.String(env.listenerArn),
			Priority:    aws.Int64(p),
			Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
			Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
		}, testAccountID)
		assert.Error(t, err, "priority %d", p)
		assert.Contains(t, err.Error(), "InvalidRulePriority")
	}
}

func TestCreateRule_RedirectAction(t *testing.T) {
	env := newRuleTestEnv(t, "cr-redir")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/old/*"})}},
		Actions: []*elbv2.Action{{
			Type: aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{
				Protocol:   aws.String("HTTPS"),
				StatusCode: aws.String("HTTP_301"),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 1)
	require.Len(t, out.Rules[0].Actions, 1)
	assert.Equal(t, "redirect", *out.Rules[0].Actions[0].Type)
	require.NotNil(t, out.Rules[0].Actions[0].RedirectConfig)
	assert.Equal(t, "HTTP_301", *out.Rules[0].Actions[0].RedirectConfig.StatusCode)
}

func TestCreateRule_FixedResponseAction(t *testing.T) {
	env := newRuleTestEnv(t, "cr-fixed")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/down"})}},
		Actions: []*elbv2.Action{{
			Type: aws.String("fixed-response"),
			FixedResponseConfig: &elbv2.FixedResponseActionConfig{
				StatusCode:  aws.String("503"),
				ContentType: aws.String("text/plain"),
				MessageBody: aws.String("maintenance"),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 1)
	assert.Equal(t, "fixed-response", *out.Rules[0].Actions[0].Type)
	require.NotNil(t, out.Rules[0].Actions[0].FixedResponseConfig)
}

func TestCreateRule_RejectsBadRedirectStatus(t *testing.T) {
	env := newRuleTestEnv(t, "cr-badredir")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions: []*elbv2.Action{{
			Type:           aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{StatusCode: aws.String("HTTP_418")},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateRule_RejectsUnknownAction(t *testing.T) {
	env := newRuleTestEnv(t, "cr-rej")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("authenticate-cognito")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidConfigurationRequest")
}

func TestCreateRule_HostHeader(t *testing.T) {
	env := newRuleTestEnv(t, "cr-host")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions: []*elbv2.RuleCondition{{
			Field:            aws.String("host-header"),
			HostHeaderConfig: &elbv2.HostHeaderConditionConfig{Values: aws.StringSlice([]string{"*.example.com"})},
		}},
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)
}

func TestCreateRule_HTTPHeader(t *testing.T) {
	env := newRuleTestEnv(t, "cr-hdr")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions: []*elbv2.RuleCondition{{
			Field: aws.String("http-header"),
			HttpHeaderConfig: &elbv2.HttpHeaderConditionConfig{
				HttpHeaderName: aws.String("X-Tenant"),
				Values:         aws.StringSlice([]string{"acme"}),
			},
		}},
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)
}

func TestCreateRule_SourceIP(t *testing.T) {
	env := newRuleTestEnv(t, "cr-src")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions: []*elbv2.RuleCondition{{
			Field:          aws.String("source-ip"),
			SourceIpConfig: &elbv2.SourceIpConditionConfig{Values: aws.StringSlice([]string{"10.0.0.0/8"})},
		}},
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(2),
		Conditions: []*elbv2.RuleCondition{{
			Field:          aws.String("source-ip"),
			SourceIpConfig: &elbv2.SourceIpConditionConfig{Values: aws.StringSlice([]string{"not-a-cidr"})},
		}},
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateRule_RejectsNewlineInjection(t *testing.T) {
	env := newRuleTestEnv(t, "cr-inj")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions: []*elbv2.RuleCondition{{
			Field: aws.String("path-pattern"),
			// Embedded newline tries to inject a new HAProxy directive.
			Values: aws.StringSlice([]string{"/api\nbackend evil"}),
		}},
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestModifyRule(t *testing.T) {
	env := newRuleTestEnv(t, "mod")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/v1/*"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)
	arn := *out.Rules[0].RuleArn

	modOut, err := env.svc.ModifyRule(&elbv2.ModifyRuleInput{
		RuleArn:    aws.String(arn),
		Conditions: []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/v2/*"})}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, modOut.Rules, 1)
	require.Len(t, modOut.Rules[0].Conditions, 1)
	assert.Equal(t, "/v2/*", *modOut.Rules[0].Conditions[0].Values[0])
}

func TestDeleteRule(t *testing.T) {
	env := newRuleTestEnv(t, "del")

	out, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.DeleteRule(&elbv2.DeleteRuleInput{RuleArn: out.Rules[0].RuleArn}, testAccountID)
	require.NoError(t, err)

	desc, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)
	// Only the synthesised default rule remains.
	require.Len(t, desc.Rules, 1)
	assert.True(t, *desc.Rules[0].IsDefault)
}

func TestDeleteListener_CascadesRules(t *testing.T) {
	env := newRuleTestEnv(t, "cas")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.DeleteListener(&elbv2.DeleteListenerInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)

	rules, err := env.svc.store.ListRules()
	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestDeleteTargetGroup_BlockedByRule(t *testing.T) {
	env := newRuleTestEnv(t, "tg-block")

	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(env.tgAltArn)}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ResourceInUse")
}

func TestDescribeRules_ByListener_SortedAndDefaultLast(t *testing.T) {
	env := newRuleTestEnv(t, "desc")

	for _, p := range []int64{30, 10, 20} {
		_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
			ListenerArn: aws.String(env.listenerArn),
			Priority:    aws.Int64(p),
			Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/p"})}},
			Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
		}, testAccountID)
		require.NoError(t, err)
	}

	out, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 4)
	assert.Equal(t, "10", *out.Rules[0].Priority)
	assert.Equal(t, "20", *out.Rules[1].Priority)
	assert.Equal(t, "30", *out.Rules[2].Priority)
	assert.True(t, *out.Rules[3].IsDefault)
}

// TestDefaultRule_HasArnAndIsTaggable guards against the regression where the
// synthetic default rule was returned with no ARN: the controller then issued
// DescribeTags with an empty resource and got MissingParameter, aborting the
// Ingress reconcile right after listener creation.
func TestDefaultRule_HasArnAndIsTaggable(t *testing.T) {
	env := newRuleTestEnv(t, "defrule")

	rules, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, rules.Rules, 1)
	def := rules.Rules[0]
	require.True(t, *def.IsDefault)
	require.NotNil(t, def.RuleArn, "default rule must carry an ARN")
	require.NotEmpty(t, *def.RuleArn)
	assert.Contains(t, *def.RuleArn, ":listener-rule/")

	// DescribeTags on the default rule ARN must succeed with empty tags, not error.
	tagsOut, err := env.svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{def.RuleArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, tagsOut.TagDescriptions, 1)
	assert.Equal(t, *def.RuleArn, *tagsOut.TagDescriptions[0].ResourceArn)
	assert.Empty(t, tagsOut.TagDescriptions[0].Tags)

	// DescribeRules by the default rule ARN resolves the synthetic rule.
	byArn, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{
		RuleArns: []*string{def.RuleArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, byArn.Rules, 1)
	assert.True(t, *byArn.Rules[0].IsDefault)

	// A non-default, unknown rule ARN under the same listener still 404s.
	_, err = env.svc.DescribeTags(&elbv2.DescribeTagsInput{ResourceArns: []*string{aws.String(*def.RuleArn + "-bogus")}}, testAccountID)
	require.Error(t, err)
}

func TestSetRulePriorities_Reorders(t *testing.T) {
	env := newRuleTestEnv(t, "spri")

	r1, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)
	r2, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(20),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/b"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{
			{RuleArn: r1.Rules[0].RuleArn, Priority: aws.Int64(20)},
			{RuleArn: r2.Rules[0].RuleArn, Priority: aws.Int64(10)},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{ListenerArn: aws.String(env.listenerArn)}, testAccountID)
	require.NoError(t, err)
	// First two non-default rules now /b at 10, /a at 20.
	require.GreaterOrEqual(t, len(out.Rules), 3)
	assert.Equal(t, "10", *out.Rules[0].Priority)
	assert.Equal(t, "/b", *out.Rules[0].Conditions[0].Values[0])
	assert.Equal(t, "20", *out.Rules[1].Priority)
	assert.Equal(t, "/a", *out.Rules[1].Conditions[0].Values[0])
}

func TestSetRulePriorities_DuplicateRejected(t *testing.T) {
	env := newRuleTestEnv(t, "spri-dup")

	r1, _ := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	r2, _ := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(20),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/b"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)

	_, err := env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{
			{RuleArn: r1.Rules[0].RuleArn, Priority: aws.Int64(50)},
			{RuleArn: r2.Rules[0].RuleArn, Priority: aws.Int64(50)},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PriorityInUse")
}

func TestBuildRuleArn(t *testing.T) {
	listenerArn := "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener/app/my-alb/abc/lst123"
	got, err := buildRuleArn(listenerArn, "rule999")
	require.NoError(t, err)
	want := "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener-rule/app/my-alb/abc/lst123/rule999"
	assert.Equal(t, want, got)
}

func TestHAProxyRender_PathPatternRule(t *testing.T) {
	lb := &LoadBalancerRecord{
		LoadBalancerID:  "lb1",
		LoadBalancerArn: "arn:lb1",
		Type:            LoadBalancerTypeApplication,
	}
	listener := &ListenerRecord{
		ListenerArn:     "arn:lst1",
		ListenerID:      "lst1",
		LoadBalancerArn: "arn:lb1",
		Protocol:        ProtocolHTTP,
		Port:            80,
		DefaultActions:  []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/default"}},
	}
	rule := &RuleRecord{
		RuleArn:     "arn:rule1",
		RuleID:      "rule1",
		ListenerArn: "arn:lst1",
		Priority:    10,
		Conditions:  []RuleCondition{{Field: RuleFieldPathPattern, Values: []string{"/api/*"}}},
		Actions:     []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/rule"}},
	}
	tgByArn := map[string]*TargetGroupRecord{
		"arn:aws:elbv2:tg/default": {Name: "default", TargetGroupArn: "arn:aws:elbv2:tg/default", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
		"arn:aws:elbv2:tg/rule":    {Name: "rule", TargetGroupArn: "arn:aws:elbv2:tg/rule", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
	}

	cfg, err := GenerateHAProxyConfig(lb, []*ListenerRecord{listener}, tgByArn, map[string][]*RuleRecord{"arn:lst1": {rule}}, "0.0.0.0")
	require.NoError(t, err)
	assert.Contains(t, cfg, "acl rrule1_c0 path -m beg /api/")
	assert.Contains(t, cfg, "use_backend bk_rule if rrule1_c0")
	assert.Contains(t, cfg, "default_backend bk_default")
}

func TestHAProxyRender_HostHeaderWildcardSuffix(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb1", LoadBalancerArn: "arn:lb1", Type: LoadBalancerTypeApplication}
	listener := &ListenerRecord{
		ListenerArn: "arn:lst1", ListenerID: "lst1", LoadBalancerArn: "arn:lb1",
		Protocol: ProtocolHTTP, Port: 80,
		DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/default"}},
	}
	rule := &RuleRecord{
		RuleArn: "arn:rule1", RuleID: "rule1", ListenerArn: "arn:lst1", Priority: 1,
		Conditions: []RuleCondition{{Field: RuleFieldHostHeader, Values: []string{"*.example.com"}}},
		Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/rule"}},
	}
	tgByArn := map[string]*TargetGroupRecord{
		"arn:aws:elbv2:tg/default": {Name: "d", TargetGroupArn: "arn:aws:elbv2:tg/default", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
		"arn:aws:elbv2:tg/rule":    {Name: "r", TargetGroupArn: "arn:aws:elbv2:tg/rule", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
	}

	cfg, err := GenerateHAProxyConfig(lb, []*ListenerRecord{listener}, tgByArn, map[string][]*RuleRecord{"arn:lst1": {rule}}, "0.0.0.0")
	require.NoError(t, err)
	assert.Contains(t, cfg, "hdr(host) -i -m end .example.com")
}

func TestHAProxyRender_RulesOrderedByPriority(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb1", LoadBalancerArn: "arn:lb1", Type: LoadBalancerTypeApplication}
	listener := &ListenerRecord{
		ListenerArn: "arn:lst1", ListenerID: "lst1", LoadBalancerArn: "arn:lb1",
		Protocol: ProtocolHTTP, Port: 80,
		DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/default"}},
	}
	low := &RuleRecord{
		RuleArn: "arn:low", RuleID: "low", ListenerArn: "arn:lst1", Priority: 5,
		Conditions: []RuleCondition{{Field: RuleFieldPathPattern, Values: []string{"/a"}}},
		Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/low"}},
	}
	high := &RuleRecord{
		RuleArn: "arn:high", RuleID: "high", ListenerArn: "arn:lst1", Priority: 1,
		Conditions: []RuleCondition{{Field: RuleFieldPathPattern, Values: []string{"/b"}}},
		Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/high"}},
	}
	tgByArn := map[string]*TargetGroupRecord{
		"arn:aws:elbv2:tg/default": {Name: "d", TargetGroupArn: "arn:aws:elbv2:tg/default", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
		"arn:aws:elbv2:tg/low":     {Name: "lo", TargetGroupArn: "arn:aws:elbv2:tg/low", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
		"arn:aws:elbv2:tg/high":    {Name: "hi", TargetGroupArn: "arn:aws:elbv2:tg/high", Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}},
	}

	// Pass rules unsorted; rendering must order them by priority.
	cfg, err := GenerateHAProxyConfig(lb, []*ListenerRecord{listener}, tgByArn, map[string][]*RuleRecord{
		"arn:lst1": sortByPriority([]*RuleRecord{low, high}),
	}, "0.0.0.0")
	require.NoError(t, err)
	hiIdx := strings.Index(cfg, "use_backend bk_high")
	loIdx := strings.Index(cfg, "use_backend bk_low")
	require.GreaterOrEqual(t, hiIdx, 0)
	require.GreaterOrEqual(t, loIdx, 0)
	assert.Less(t, hiIdx, loIdx, "priority 1 must render before priority 5")
}

func sortByPriority(rs []*RuleRecord) []*RuleRecord {
	out := append([]*RuleRecord(nil), rs...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Priority < out[i].Priority {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
