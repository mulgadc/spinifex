//test:in-package — drives ELBv2_Request through the gateway's unexported test
// helpers (scopedPolicyGateway, withTestIdentity) and auth context keys.

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func elbv2LB(name, id string) string {
	return arn.FormatELBv2LoadBalancer(authzRegion, authzAccountID, name, id, "application")
}

func elbv2TG(name, id string) string {
	return arn.FormatELBv2TargetGroup(authzRegion, authzAccountID, name, id)
}

func elbv2Listener(lbName, lbID, id string) string {
	return arn.FormatELBv2Listener(authzRegion, authzAccountID, lbName, lbID, id, "application")
}

// dispatchELBv2 drives the gateway with no NATS connection. A permitted request
// therefore reaches the NATS guard and fails there, which is what proves the
// policy check ran ahead of the resource having to exist.
func dispatchELBv2(t *testing.T, gw *GatewayConfig, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "elasticloadbalancing")
	ctx = context.WithValue(ctx, ctxAccountID, authzAccountID)
	return gw.ELBv2_Request(httptest.NewRecorder(), withTestIdentity(req.WithContext(ctx)))
}

// TestELBv2Request_ScopedDenyFires is the bypass this work closes. An operator
// fences a production load balancer; before the resolver the fence was inert
// and DeleteLoadBalancer against it was permitted with nothing logged.
func TestELBv2Request_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:*", "*"),
		statement("Deny", "elasticloadbalancing:DeleteLoadBalancer", elbv2LB("prod", "abc123")),
	)

	assertDenied(t, dispatchELBv2(t, gw, "Action=DeleteLoadBalancer&LoadBalancerArn="+elbv2LB("prod", "abc123")))
	assertPermitted(t, dispatchELBv2(t, gw, "Action=DeleteLoadBalancer&LoadBalancerArn="+elbv2LB("dev", "def456")))
}

// TestELBv2Request_ScopedAllowGrants is the other half: a least-privilege
// policy used to deny everything, so the only working shape was Resource "*".
// The sibling is denied rather than not-found, because the gate runs first.
func TestELBv2Request_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:RegisterTargets", elbv2TG("web", "def456")),
	)

	assertPermitted(t, dispatchELBv2(t, gw, "Action=RegisterTargets&TargetGroupArn="+elbv2TG("web", "def456")))
	assertDenied(t, dispatchELBv2(t, gw, "Action=RegisterTargets&TargetGroupArn="+elbv2TG("other", "def456")))
}

// A create-time fence is what a name-prefix convention depends on, and it can
// only fire because the create resolves to the name it asks for.
func TestELBv2Request_CreateScopedByName(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:*", "*"),
		statement("Deny", "elasticloadbalancing:CreateLoadBalancer",
			"arn:aws:elasticloadbalancing:*:*:loadbalancer/app/prod-*"),
	)

	assertDenied(t, dispatchELBv2(t, gw, "Action=CreateLoadBalancer&Name=prod-web"))
	assertPermitted(t, dispatchELBv2(t, gw, "Action=CreateLoadBalancer&Name=dev-web"))
}

// A listener is denied on its target group even though its load balancer is
// permitted: every resource AWS evaluates is evaluated here too.
func TestELBv2Request_MultiResourceParity(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:*", "*"),
		statement("Deny", "elasticloadbalancing:CreateListener", elbv2TG("restricted", "1")),
	)

	assertDenied(t, dispatchELBv2(t, gw,
		"Action=CreateListener&LoadBalancerArn="+elbv2LB("prod", "abc123")+
			"&DefaultActions.member.1.Type=forward&DefaultActions.member.1.TargetGroupArn="+elbv2TG("restricted", "1")))
	assertPermitted(t, dispatchELBv2(t, gw,
		"Action=CreateListener&LoadBalancerArn="+elbv2LB("prod", "abc123")+
			"&DefaultActions.member.1.Type=forward&DefaultActions.member.1.TargetGroupArn="+elbv2TG("allowed", "2")))
}

// A redirect-only default action names no target group, so the call stays
// scoped to the load balancer rather than widening to "*".
func TestELBv2Request_RedirectActionDoesNotWiden(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:CreateListener", elbv2LB("prod", "abc123")),
		statement("Allow", "elasticloadbalancing:CreateListener",
			"arn:aws:elasticloadbalancing:*:*:listener/app/prod/abc123/*"),
	)

	assertPermitted(t, dispatchELBv2(t, gw,
		"Action=CreateListener&LoadBalancerArn="+elbv2LB("prod", "abc123")+
			"&DefaultActions.member.1.Type=redirect"))
}

// One fenced member fails the batch whole, which is AWS's combining rule.
func TestELBv2Request_DenyOneTagMemberFailsTheBatch(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "elasticloadbalancing:*", "*"),
		statement("Deny", "elasticloadbalancing:AddTags", elbv2LB("prod", "abc123")),
	)

	assertDenied(t, dispatchELBv2(t, gw,
		"Action=AddTags&ResourceArns.member.1="+elbv2TG("web", "1")+
			"&ResourceArns.member.2="+elbv2LB("prod", "abc123")+
			"&Tags.member.1.Key=env&Tags.member.1.Value=prod"))
	assertPermitted(t, dispatchELBv2(t, gw,
		"Action=AddTags&ResourceArns.member.1="+elbv2TG("web", "1")+
			"&Tags.member.1.Key=env&Tags.member.1.Value=prod"))
}

// An ARN naming another account is rejected at the gate rather than authorized
// verbatim and left for the handler to fail as a not-found.
func TestELBv2Request_ForeignAccountARNRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "elasticloadbalancing:*", "*"))

	err := dispatchELBv2(t, gw, "Action=DeleteLoadBalancer&LoadBalancerArn="+
		arn.FormatELBv2LoadBalancer(authzRegion, "999999999999", "prod", "abc123", "application"))
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)

	err = dispatchELBv2(t, gw, "Action=DeleteLoadBalancer&LoadBalancerArn="+
		arn.FormatELBv2LoadBalancer("us-east-1", authzAccountID, "prod", "abc123", "application"))
	require.Error(t, err)
	code, ok = awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
}

// An absent identifier authorizes account-wide, leaving the validation fault
// where it belongs: with the handler.
func TestELBv2Request_AbsentIdentifierStaysHandlerFault(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "elasticloadbalancing:*", "*"))

	assertPermitted(t, dispatchELBv2(t, gw, "Action=DeleteLoadBalancer"))
	assertDenied(t, dispatchELBv2(t,
		scopedPolicyGateway(statement("Allow", "elasticloadbalancing:*", elbv2LB("prod", "abc123"))),
		"Action=DeleteLoadBalancer"))
}

// A missing account ID returns InternalError, which is the code the policy gate
// returned before the read was hoisted above it.
func TestELBv2Request_MissingAccountIDIsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "elasticloadbalancing:*", "*"))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DescribeLoadBalancers"))
	ctx := context.WithValue(req.Context(), ctxService, "elasticloadbalancing")
	err := gw.ELBv2_Request(httptest.NewRecorder(), withTestIdentity(req.WithContext(ctx)))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// Non-regression: an account-wide Allow still permits every scoped action once
// a real ARN is passed, so scoping did not quietly deny working policies.
func TestELBv2Request_AccountWideAllowStillPermitsEveryScopedAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "elasticloadbalancing:*", "*"))

	lb := elbv2LB("prod", "abc123")
	tg := elbv2TG("web", "def456")
	listener := elbv2Listener("prod", "abc123", "l1")
	rule := arn.FormatELBv2Resource(authzRegion, authzAccountID, arn.ELBv2ListenerRule, "app/prod/abc123/l1/r1")

	bodies := map[string]string{
		"CreateLoadBalancer":             "Action=CreateLoadBalancer&Name=prod",
		"DeleteLoadBalancer":             "Action=DeleteLoadBalancer&LoadBalancerArn=" + lb,
		"ModifyLoadBalancerAttributes":   "Action=ModifyLoadBalancerAttributes&LoadBalancerArn=" + lb,
		"DescribeLoadBalancerAttributes": "Action=DescribeLoadBalancerAttributes&LoadBalancerArn=" + lb,
		"SetSecurityGroups":              "Action=SetSecurityGroups&LoadBalancerArn=" + lb,
		"SetSubnets":                     "Action=SetSubnets&LoadBalancerArn=" + lb,
		"SetIpAddressType":               "Action=SetIpAddressType&LoadBalancerArn=" + lb,
		"CreateTargetGroup":              "Action=CreateTargetGroup&Name=web",
		"DeleteTargetGroup":              "Action=DeleteTargetGroup&TargetGroupArn=" + tg,
		"ModifyTargetGroup":              "Action=ModifyTargetGroup&TargetGroupArn=" + tg,
		"ModifyTargetGroupAttributes":    "Action=ModifyTargetGroupAttributes&TargetGroupArn=" + tg,
		"DescribeTargetGroupAttributes":  "Action=DescribeTargetGroupAttributes&TargetGroupArn=" + tg,
		"RegisterTargets":                "Action=RegisterTargets&TargetGroupArn=" + tg,
		"DeregisterTargets":              "Action=DeregisterTargets&TargetGroupArn=" + tg,
		"DescribeTargetHealth":           "Action=DescribeTargetHealth&TargetGroupArn=" + tg,
		"CreateListener":                 "Action=CreateListener&LoadBalancerArn=" + lb,
		"ModifyListener":                 "Action=ModifyListener&ListenerArn=" + listener,
		"DeleteListener":                 "Action=DeleteListener&ListenerArn=" + listener,
		"AddListenerCertificates":        "Action=AddListenerCertificates&ListenerArn=" + listener,
		"RemoveListenerCertificates":     "Action=RemoveListenerCertificates&ListenerArn=" + listener,
		"DescribeListenerCertificates":   "Action=DescribeListenerCertificates&ListenerArn=" + listener,
		"DescribeListenerAttributes":     "Action=DescribeListenerAttributes&ListenerArn=" + listener,
		"ModifyListenerAttributes":       "Action=ModifyListenerAttributes&ListenerArn=" + listener,
		"CreateRule":                     "Action=CreateRule&ListenerArn=" + listener,
		"ModifyRule":                     "Action=ModifyRule&RuleArn=" + rule,
		"DeleteRule":                     "Action=DeleteRule&RuleArn=" + rule,
		"SetRulePriorities":              "Action=SetRulePriorities&RulePriorities.member.1.RuleArn=" + rule,
		"AddTags":                        "Action=AddTags&ResourceArns.member.1=" + lb,
		"RemoveTags":                     "Action=RemoveTags&ResourceArns.member.1=" + lb,
		"DescribeTags":                   "Action=DescribeTags&ResourceArns.member.1=" + lb,
	}

	// Every scoped action is covered, so a new one cannot land here untested.
	for action := range elbv2Actions {
		if _, unscoped := elbv2UnscopedActions[action]; unscoped {
			continue
		}
		require.Contains(t, bodies, action, "add %q to the non-regression table", action)
	}

	for action, body := range bodies {
		t.Run(action, func(t *testing.T) {
			assertPermitted(t, dispatchELBv2(t, gw, body))
		})
	}
}

// The seven actions AWS documents no resource type for, listed here so the
// non-regression table above can assert it covers everything else.
var elbv2UnscopedActions = map[string]struct{}{
	"DescribeLoadBalancers": {},
	"DescribeTargetGroups":  {},
	"DescribeListeners":     {},
	"DescribeRules":         {},
	"DescribeSSLPolicies":   {},
	"LBAgentHeartbeat":      {},
	"GetLBConfig":           {},
}
