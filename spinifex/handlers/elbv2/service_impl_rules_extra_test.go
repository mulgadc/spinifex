package handlers_elbv2

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateRule_InputValidation(t *testing.T) {
	env := newRuleTestEnv(t, "cr-val")

	cases := []struct {
		name  string
		input *elbv2.CreateRuleInput
		want  string
	}{
		{"nil input", nil, "MissingParameter"},
		{"missing listener arn", &elbv2.CreateRuleInput{Priority: aws.Int64(1)}, "MissingParameter"},
		{"missing priority", &elbv2.CreateRuleInput{ListenerArn: aws.String(env.listenerArn)}, "MissingParameter"},
		{"unknown listener", &elbv2.CreateRuleInput{ListenerArn: aws.String("arn:does-not-exist"), Priority: aws.Int64(1)}, "ListenerNotFound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.CreateRule(tc.input, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestCreateRule_WrongAccount(t *testing.T) {
	env := newRuleTestEnv(t, "cr-acct")
	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, "other-account")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListenerNotFound")
}

func TestModifyRule_InputValidation(t *testing.T) {
	env := newRuleTestEnv(t, "mod-val")
	_, err := env.svc.ModifyRule(nil, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.ModifyRule(&elbv2.ModifyRuleInput{RuleArn: aws.String("")}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.ModifyRule(&elbv2.ModifyRuleInput{RuleArn: aws.String("arn:nope")}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RuleNotFound")
}

func TestModifyRule_RewriteActions(t *testing.T) {
	env := newRuleTestEnv(t, "mod-act")
	cr, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(5),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := env.svc.ModifyRule(&elbv2.ModifyRuleInput{
		RuleArn: cr.Rules[0].RuleArn,
		Actions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, env.tgArn, *out.Rules[0].Actions[0].TargetGroupArn)
}

func TestDeleteRule_InputValidation(t *testing.T) {
	env := newRuleTestEnv(t, "del-val")
	_, err := env.svc.DeleteRule(nil, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.DeleteRule(&elbv2.DeleteRuleInput{RuleArn: aws.String("")}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	// AWS ELBv2 delete is idempotent: absent rule -> success, not NotFound.
	out, err := env.svc.DeleteRule(&elbv2.DeleteRuleInput{RuleArn: aws.String("arn:none")}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, out)
}

func TestDescribeRules_InputValidation(t *testing.T) {
	env := newRuleTestEnv(t, "desc-val")
	_, err := env.svc.DescribeRules(nil, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.DescribeRules(&elbv2.DescribeRulesInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")
}

func TestDescribeRules_ByRuleArns(t *testing.T) {
	env := newRuleTestEnv(t, "desc-arns")
	cr, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := env.svc.DescribeRules(&elbv2.DescribeRulesInput{
		RuleArns: []*string{cr.Rules[0].RuleArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Rules, 1)
	assert.Equal(t, *cr.Rules[0].RuleArn, *out.Rules[0].RuleArn)

	_, err = env.svc.DescribeRules(&elbv2.DescribeRulesInput{
		RuleArns: []*string{aws.String("arn:nope")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RuleNotFound")
}

func TestSetRulePriorities_InputValidation(t *testing.T) {
	env := newRuleTestEnv(t, "spri-val")
	_, err := env.svc.SetRulePriorities(nil, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{nil},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")
}

func TestSetRulePriorities_InvalidPriority(t *testing.T) {
	env := newRuleTestEnv(t, "spri-bad")
	cr, _ := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)

	_, err := env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{
			{RuleArn: cr.Rules[0].RuleArn, Priority: aws.Int64(0)},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidRulePriority")

	_, err = env.svc.SetRulePriorities(&elbv2.SetRulePrioritiesInput{
		RulePriorities: []*elbv2.RulePriorityPair{
			{RuleArn: aws.String("arn:none"), Priority: aws.Int64(1)},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RuleNotFound")
}

func TestValidateConditions_TooMany(t *testing.T) {
	in := make([]*elbv2.RuleCondition, MaxConditionsPerRule+1)
	for i := range in {
		in[i] = &elbv2.RuleCondition{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}
	}
	_, err := validateAndConvertConditions(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestValidateConditions_Empty(t *testing.T) {
	_, err := validateAndConvertConditions(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")
}

func TestValidateConditions_InvalidField(t *testing.T) {
	_, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("unknown-field"), Values: aws.StringSlice([]string{"x"})},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestValidateConditions_HTTPHeader(t *testing.T) {
	// Missing HttpHeaderConfig.
	_, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-header")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	// Empty name.
	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-header"), HttpHeaderConfig: &elbv2.HttpHeaderConditionConfig{HttpHeaderName: aws.String(""), Values: aws.StringSlice([]string{"v"})}},
	})
	require.Error(t, err)

	// Invalid value (control byte).
	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-header"), HttpHeaderConfig: &elbv2.HttpHeaderConditionConfig{HttpHeaderName: aws.String("X-Test"), Values: aws.StringSlice([]string{"bad\x01value"})}},
	})
	require.Error(t, err)

	// Valid.
	out, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-header"), HttpHeaderConfig: &elbv2.HttpHeaderConditionConfig{HttpHeaderName: aws.String("X-Test"), Values: aws.StringSlice([]string{"yes"})}},
	})
	require.NoError(t, err)
	assert.Equal(t, "X-Test", out[0].HTTPHeaderName)
}

func TestValidateConditions_Method(t *testing.T) {
	_, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-request-method")},
	})
	require.Error(t, err)

	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-request-method"), HttpRequestMethodConfig: &elbv2.HttpRequestMethodConditionConfig{Values: aws.StringSlice([]string{"BREW"})}},
	})
	require.Error(t, err)

	out, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("http-request-method"), HttpRequestMethodConfig: &elbv2.HttpRequestMethodConditionConfig{Values: aws.StringSlice([]string{"POST"})}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"POST"}, out[0].Values)
}

func TestValidateConditions_QueryString(t *testing.T) {
	_, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("query-string")},
	})
	require.Error(t, err)

	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("query-string"), QueryStringConfig: &elbv2.QueryStringConditionConfig{Values: []*elbv2.QueryStringKeyValuePair{{Key: aws.String("k")}}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	out, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("query-string"), QueryStringConfig: &elbv2.QueryStringConditionConfig{Values: []*elbv2.QueryStringKeyValuePair{
			{Key: aws.String("k"), Value: aws.String("v")},
			{Value: aws.String("just-value")},
		}}},
	})
	require.NoError(t, err)
	require.Len(t, out[0].QueryStringKVs, 2)
	assert.Equal(t, "k", out[0].QueryStringKVs[0].Key)
	assert.Equal(t, "", out[0].QueryStringKVs[1].Key)
}

func TestValidateConditions_SourceIP(t *testing.T) {
	_, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("source-ip")},
	})
	require.Error(t, err)

	_, err = validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("source-ip"), SourceIpConfig: &elbv2.SourceIpConditionConfig{Values: aws.StringSlice([]string{"not-a-cidr"})}},
	})
	require.Error(t, err)

	out, err := validateAndConvertConditions([]*elbv2.RuleCondition{
		{Field: aws.String("source-ip"), SourceIpConfig: &elbv2.SourceIpConditionConfig{Values: aws.StringSlice([]string{"10.0.0.0/8"})}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8"}, out[0].Values)
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		in       string
		wantFlag string
		wantLit  string
	}{
		{"/api", "str", "/api"},
		{"/api*", "beg", "/api"},
		{"*.example.com", "end", ".example.com"},
		{"*foo*", "sub", "foo"},
		{"a?b", "sub", "a?b"},
		{"*", "sub", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			flag, lit := wildcardMatch(tc.in)
			assert.Equal(t, tc.wantFlag, flag)
			assert.Equal(t, tc.wantLit, lit)
		})
	}
}

func TestValidateRenderSafe(t *testing.T) {
	require.NoError(t, validateRenderSafe("/api/v1"))
	for _, bad := range []string{"", "a b", "a\tb", "a\nb", "a\rb", "a#b", `a"b`} {
		err := validateRenderSafe(bad)
		require.Errorf(t, err, "expected reject %q", bad)
	}
}

// TestHAProxyRender_AllConditionFields exercises every supported field-type
// ACL emission path in one render. Each rule uses a distinct backend so the
// match expressions can be located in the generated config.
func TestHAProxyRender_AllConditionFields(t *testing.T) {
	lb := &LoadBalancerRecord{LoadBalancerID: "lb1", LoadBalancerArn: "arn:lb1", Type: LoadBalancerTypeApplication}
	listener := &ListenerRecord{
		ListenerArn: "arn:lst1", ListenerID: "lst1", LoadBalancerArn: "arn:lb1",
		Protocol: ProtocolHTTP, Port: 80,
		DefaultActions: []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/default"}},
	}
	mkTG := func(arn, name string) *TargetGroupRecord {
		return &TargetGroupRecord{Name: name, TargetGroupArn: arn, Protocol: ProtocolHTTP, HealthCheck: HealthCheckConfig{Path: "/", Matcher: "200", IntervalSeconds: 30, UnhealthyThreshold: 2, HealthyThreshold: 5}}
	}
	tgByArn := map[string]*TargetGroupRecord{
		"arn:aws:elbv2:tg/default": mkTG("arn:aws:elbv2:tg/default", "default"),
		"arn:aws:elbv2:tg/h":       mkTG("arn:aws:elbv2:tg/h", "h"),
		"arn:aws:elbv2:tg/m":       mkTG("arn:aws:elbv2:tg/m", "m"),
		"arn:aws:elbv2:tg/q":       mkTG("arn:aws:elbv2:tg/q", "q"),
		"arn:aws:elbv2:tg/ip":      mkTG("arn:aws:elbv2:tg/ip", "ip"),
	}
	rules := []*RuleRecord{
		{RuleArn: "arn:r-h", RuleID: "rh", ListenerArn: "arn:lst1", Priority: 1,
			Conditions: []RuleCondition{{Field: RuleFieldHTTPHeader, HTTPHeaderName: "X-Color", Values: []string{"red", "blue"}}},
			Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/h"}}},
		{RuleArn: "arn:r-m", RuleID: "rm", ListenerArn: "arn:lst1", Priority: 2,
			Conditions: []RuleCondition{{Field: RuleFieldHTTPRequestMethod, Values: []string{"POST", "PUT"}}},
			Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/m"}}},
		{RuleArn: "arn:r-q", RuleID: "rq", ListenerArn: "arn:lst1", Priority: 3,
			Conditions: []RuleCondition{{Field: RuleFieldQueryString, QueryStringKVs: []RuleQueryStringKV{{Key: "lang", Value: "en"}, {Value: "trace"}}}},
			Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/q"}}},
		{RuleArn: "arn:r-ip", RuleID: "rip", ListenerArn: "arn:lst1", Priority: 4,
			Conditions: []RuleCondition{{Field: RuleFieldSourceIP, Values: []string{"10.0.0.0/8"}}},
			Actions:    []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elbv2:tg/ip"}}},
	}

	cfg, err := GenerateHAProxyConfig(lb, []*ListenerRecord{listener}, tgByArn,
		map[string][]*RuleRecord{"arn:lst1": rules}, "0.0.0.0")
	require.NoError(t, err)

	for _, want := range []string{
		"hdr(X-Color) -i -m str red",
		"hdr(X-Color) -i -m str blue",
		"method POST",
		"method PUT",
		"urlp(lang) -m str en",
		"url_sub trace",
		"src 10.0.0.0/8",
		"use_backend bk_h if",
		"use_backend bk_m if",
		"use_backend bk_q if",
		"use_backend bk_ip if",
	} {
		assert.Truef(t, strings.Contains(cfg, want), "expected %q in config; got:\n%s", want, cfg)
	}
}

func TestHAProxyRender_UnsupportedField(t *testing.T) {
	_, err := buildHAProxyACLExprs(RuleCondition{Field: "nonsense", Values: []string{"x"}})
	require.Error(t, err)
}

func TestBuildHAProxyRule_RejectsNonForward(t *testing.T) {
	_, err := buildHAProxyRule(&RuleRecord{
		RuleID:  "r1",
		Actions: []ListenerAction{{Type: "redirect"}},
	})
	require.Error(t, err)
}

// TestTargetGroupsForLB_IncludesRuleTGs guards against regressing the
// rule-TG visibility bug: without rule-action enumeration, the health
// checker cannot resolve rule-only TGs back to a TG record and never
// transitions them from initial -> healthy. Reproduces the e2e failure
// where tgB (referenced only by a CreateRule action) timed out at 0/1
// healthy because TargetGroupsForLB walked only DefaultActions.
func TestTargetGroupsForLB_IncludesRuleTGs(t *testing.T) {
	env := newRuleTestEnv(t, "tgs-rule")
	_, err := env.svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(env.listenerArn),
		Priority:    aws.Int64(1),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/a"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgAltArn)}},
	}, testAccountID)
	require.NoError(t, err)

	lb, err := env.svc.store.GetLoadBalancerByArn(env.lbArn)
	require.NoError(t, err)
	require.NotNil(t, lb)

	tgs, err := env.svc.store.TargetGroupsForLB(lb.LoadBalancerID)
	require.NoError(t, err)

	arns := make(map[string]bool, len(tgs))
	for _, tg := range tgs {
		arns[tg.TargetGroupArn] = true
	}
	assert.Truef(t, arns[env.tgArn], "default TG missing: have %v", arns)
	assert.Truef(t, arns[env.tgAltArn], "rule TG missing: have %v", arns)
}
