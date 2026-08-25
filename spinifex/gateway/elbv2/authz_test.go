package gateway_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzRegion  = "ap-southeast-2"
	authzAccount = "123456789012"
)

func lbARN(name, id string) string {
	return arn.FormatELBv2LoadBalancer(authzRegion, authzAccount, name, id, "application")
}

func tgARN(name, id string) string {
	return arn.FormatELBv2TargetGroup(authzRegion, authzAccount, name, id)
}

func listenerARN(lbName, lbID, id string) string {
	return arn.FormatELBv2Listener(authzRegion, authzAccount, lbName, lbID, id, "application")
}

func resolveARNs(t *testing.T, action string, input any) []string {
	t.Helper()
	resources, err := ResourceARNs(action, authzRegion, authzAccount, input)
	require.NoError(t, err)
	return resources
}

func TestResourceARNs_UnknownAction(t *testing.T) {
	_, err := ResourceARNs("NotAnAction", authzRegion, authzAccount, &elbv2.DeleteLoadBalancerInput{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// Each resource class is asserted against the exact ARN, so a gate that
// authorizes a differently-spelled ARN than the handler stores fails here.
func TestResourceARNs_PerResourceClass(t *testing.T) {
	assert.Equal(t, []string{lbARN("prod", "abc123")},
		resolveARNs(t, "DeleteLoadBalancer", &elbv2.DeleteLoadBalancerInput{
			LoadBalancerArn: aws.String(lbARN("prod", "abc123")),
		}))

	assert.Equal(t, []string{tgARN("web", "def456")},
		resolveARNs(t, "RegisterTargets", &elbv2.RegisterTargetsInput{
			TargetGroupArn: aws.String(tgARN("web", "def456")),
		}))

	assert.Equal(t, []string{listenerARN("prod", "abc123", "l1")},
		resolveARNs(t, "DeleteListener", &elbv2.DeleteListenerInput{
			ListenerArn: aws.String(listenerARN("prod", "abc123", "l1")),
		}))

	ruleARN := arn.FormatELBv2Resource(authzRegion, authzAccount, arn.ELBv2ListenerRule, "app/prod/abc123/l1/r1")
	assert.Equal(t, []string{ruleARN},
		resolveARNs(t, "DeleteRule", &elbv2.DeleteRuleInput{RuleArn: aws.String(ruleARN)}))
}

// The four creates name a resource that does not exist, so the short id is a
// wildcard under a path the parent or the requested name already fixes.
func TestResourceARNs_Creates(t *testing.T) {
	assert.Equal(t,
		[]string{"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/app/prod/*"},
		resolveARNs(t, "CreateLoadBalancer", &elbv2.CreateLoadBalancerInput{Name: aws.String("prod")}))

	assert.Equal(t,
		[]string{"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/net/edge/*"},
		resolveARNs(t, "CreateLoadBalancer", &elbv2.CreateLoadBalancerInput{
			Name: aws.String("edge"), Type: aws.String("network"),
		}))

	assert.Equal(t,
		[]string{"arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/web/*"},
		resolveARNs(t, "CreateTargetGroup", &elbv2.CreateTargetGroupInput{Name: aws.String("web")}))

	assert.Equal(t,
		[]string{lbARN("prod", "abc123"), "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener/app/prod/abc123/*"},
		resolveARNs(t, "CreateListener", &elbv2.CreateListenerInput{
			LoadBalancerArn: aws.String(lbARN("prod", "abc123")),
		}))

	assert.Equal(t,
		[]string{listenerARN("prod", "abc123", "l1"), "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:listener-rule/app/prod/abc123/l1/*"},
		resolveARNs(t, "CreateRule", &elbv2.CreateRuleInput{
			ListenerArn: aws.String(listenerARN("prod", "abc123", "l1")),
		}))
}

// The derived listener ARN is the shared builder's output with the short id
// replaced by "*", which is what makes a create-time name-prefix fence work.
func TestResourceARNs_DerivedListenerMatchesBuilder(t *testing.T) {
	stored := arn.FormatELBv2Listener(authzRegion, authzAccount, "prod", "abc123", "l7", "application")
	resources := resolveARNs(t, "CreateListener", &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbARN("prod", "abc123")),
	})
	require.Len(t, resources, 2)
	assert.Equal(t, arn.FormatELBv2Listener(authzRegion, authzAccount, "prod", "abc123", "*", "application"), resources[1])
	assert.NotEqual(t, stored, resources[1])
}

// A listener's target groups are authorized alongside it, and a redirect-only
// action names none, which leaves the call scoped rather than widening it.
func TestResourceARNs_ListenerTargetGroups(t *testing.T) {
	withTG := resolveARNs(t, "CreateListener", &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbARN("prod", "abc123")),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String(tgARN("web", "def456"))},
		},
	})
	assert.Contains(t, withTG, tgARN("web", "def456"))

	redirect := resolveARNs(t, "CreateListener", &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbARN("prod", "abc123")),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("redirect")}},
	})
	assert.NotContains(t, redirect, "*")
	assert.Len(t, redirect, 2)
}

// A weighted forward names its target groups inside ForwardConfig, which AWS
// evaluates the same way as the flat TargetGroupArn.
func TestResourceARNs_ForwardConfigTargetGroups(t *testing.T) {
	resources := resolveARNs(t, "ModifyRule", &elbv2.ModifyRuleInput{
		RuleArn: aws.String(arn.FormatELBv2Resource(authzRegion, authzAccount, arn.ELBv2ListenerRule, "app/prod/abc123/l1/r1")),
		Actions: []*elbv2.Action{{
			Type: aws.String("forward"),
			ForwardConfig: &elbv2.ForwardActionConfig{
				TargetGroups: []*elbv2.TargetGroupTuple{
					{TargetGroupArn: aws.String(tgARN("blue", "1"))},
					{TargetGroupArn: aws.String(tgARN("green", "2"))},
				},
			},
		}},
	})
	assert.Contains(t, resources, tgARN("blue", "1"))
	assert.Contains(t, resources, tgARN("green", "2"))
}

func TestResourceARNs_ListsEveryMember(t *testing.T) {
	assert.Equal(t, []string{tgARN("a", "1"), tgARN("b", "2")},
		resolveARNs(t, "AddTags", &elbv2.AddTagsInput{
			ResourceArns: []*string{aws.String(tgARN("a", "1")), aws.String(tgARN("b", "2"))},
		}))

	assert.Equal(t, []string{lbARN("one", "1"), lbARN("two", "2")},
		resolveARNs(t, "DescribeTags", &elbv2.DescribeTagsInput{
			ResourceArns: []*string{aws.String(lbARN("one", "1")), aws.String(lbARN("two", "2"))},
		}))

	rule := func(id string) string {
		return arn.FormatELBv2Resource(authzRegion, authzAccount, arn.ELBv2ListenerRule, "app/prod/abc123/l1/"+id)
	}
	assert.Equal(t, []string{rule("r1"), rule("r2")},
		resolveARNs(t, "SetRulePriorities", &elbv2.SetRulePrioritiesInput{
			RulePriorities: []*elbv2.RulePriorityPair{
				{RuleArn: aws.String(rule("r1")), Priority: aws.Int64(1)},
				{RuleArn: aws.String(rule("r2")), Priority: aws.Int64(2)},
			},
		}))
}

// An identifier the request omits leaves the call account-wide, so the
// resulting MissingParameter stays the handler's to report.
func TestResourceARNs_AbsentIdentifier(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolveARNs(t, "DeleteLoadBalancer", &elbv2.DeleteLoadBalancerInput{}))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "CreateTargetGroup", &elbv2.CreateTargetGroupInput{}))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "AddTags", &elbv2.AddTagsInput{}))
	assert.Equal(t, []string{"*"}, resolveARNs(t, "SetRulePriorities", &elbv2.SetRulePrioritiesInput{}))
}

// The seven account-wide actions are explicit entries, not omissions.
func TestResourceARNs_AccountWideActions(t *testing.T) {
	for _, action := range []string{
		"DescribeLoadBalancers", "DescribeTargetGroups", "DescribeListeners",
		"DescribeRules", "DescribeSSLPolicies", "LBAgentHeartbeat", "GetLBConfig",
	} {
		assert.True(t, HasScope(action), action)
		assert.Equal(t, []string{"*"}, resolveARNs(t, action, &elbv2.DescribeLoadBalancersInput{}), action)
	}
}

// A caller's spelling is parsed, not passed through: authorizing an ARN that
// names another account lets the gate and the handler address different objects.
func TestResourceARNs_RejectsForeignARNs(t *testing.T) {
	rejected := map[string]string{
		"another account":    arn.FormatELBv2LoadBalancer(authzRegion, "999999999999", "prod", "abc", "application"),
		"another region":     arn.FormatELBv2LoadBalancer("us-east-1", authzAccount, "prod", "abc", "application"),
		"another partition":  "arn:aws-cn:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/app/prod/abc",
		"another service":    "arn:aws:ec2:ap-southeast-2:123456789012:instance/i-abc",
		"unknown type":       "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:widget/prod/abc",
		"not an arn at all":  "prod",
		"no resource at all": "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:loadbalancer/",
	}
	for name, value := range rejected {
		_, err := ResourceARNs("DeleteLoadBalancer", authzRegion, authzAccount, &elbv2.DeleteLoadBalancerInput{
			LoadBalancerArn: aws.String(value),
		})
		require.Error(t, err, name)
		code, ok := awserrors.ResolveErrorCode(err)
		require.True(t, ok, name)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, name)
	}
}

// A parent of the wrong type cannot derive a child, and must not silently
// produce an ARN naming a resource nobody asked for.
func TestResourceARNs_DerivedChildRejectsWrongParent(t *testing.T) {
	_, err := ResourceARNs("CreateListener", authzRegion, authzAccount, &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(tgARN("web", "def456")),
	})
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}
