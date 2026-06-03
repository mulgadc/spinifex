package handlers_elbv2

import (
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_ELBv2DeleteIdempotentOnAbsent enforces Common Resource Lifecycle
// Contract rule #1 (idempotent delete) across the ELBv2 surface: deleting an
// absent resource is success, not a NotFound error. This mirrors AWS ELBv2
// delete semantics and is required for tofu destroy to converge on retry.
//
// Every ELBv2 delete endpoint MUST have a case here. When a new delete
// endpoint lands, add it; a missing case is an idempotency gap.
func TestRLC1_ELBv2DeleteIdempotentOnAbsent(t *testing.T) {
	const arnBase = "arn:aws:elasticloadbalancing:us-east-1:123456789012:"

	cases := []struct {
		name string
		call func(svc *ELBv2ServiceImpl) (any, error)
	}{
		{"DeleteLoadBalancer", func(svc *ELBv2ServiceImpl) (any, error) {
			return svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
				LoadBalancerArn: aws.String(arnBase + "loadbalancer/app/absent/0000000000000000"),
			}, testAccountID)
		}},
		{"DeleteTargetGroup", func(svc *ELBv2ServiceImpl) (any, error) {
			return svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
				TargetGroupArn: aws.String(arnBase + "targetgroup/absent/0000000000000000"),
			}, testAccountID)
		}},
		{"DeleteListener", func(svc *ELBv2ServiceImpl) (any, error) {
			return svc.DeleteListener(&elbv2.DeleteListenerInput{
				ListenerArn: aws.String(arnBase + "listener/app/absent/0000000000000000/1111111111111111"),
			}, testAccountID)
		}},
		{"DeleteRule", func(svc *ELBv2ServiceImpl) (any, error) {
			return svc.DeleteRule(&elbv2.DeleteRuleInput{
				RuleArn: aws.String(arnBase + "listener-rule/app/absent/0000000000000000/1111111111111111/2222222222222222"),
			}, testAccountID)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupTestService(t)
			out, err := tc.call(svc)
			require.NoErrorf(t, err, "%s on an absent resource must return success (RLC rule #1)", tc.name)
			assert.NotNilf(t, out, "%s must return a non-nil output on absent", tc.name)
		})
	}
}

// TestRLC2_ELBv2NoOrphanAfterDeleteLB enforces ADR-0002 §5 no-orphan
// completeness: after DeleteLoadBalancer, no listener or rule owned by that LB
// may survive in the store. Uses two listeners (each with a rule) so a
// single-listener cascade can't pass by accident.
func TestRLC2_ELBv2NoOrphanAfterDeleteLB(t *testing.T) {
	svc := setupTestService(t)

	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("rlc2-lb")}, testAccountID)
	require.NoError(t, err)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("rlc2-tg")}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	for i, port := range []int64{80, 8080} {
		lst, err := svc.CreateListener(&elbv2.CreateListenerInput{
			LoadBalancerArn: lbArn,
			Protocol:        aws.String("HTTP"),
			Port:            aws.Int64(port),
			DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
		}, testAccountID)
		require.NoError(t, err)

		_, err = svc.CreateRule(&elbv2.CreateRuleInput{
			ListenerArn: lst.Listeners[0].ListenerArn,
			Priority:    aws.Int64(int64(10 + i)),
			Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/api/*"})}},
			Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
		}, testAccountID)
		require.NoError(t, err)
	}

	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)

	listeners, err := svc.store.ListListenersByLB(*lbArn)
	require.NoError(t, err)
	require.Emptyf(t, listeners, "ADR-0002 §5 no-orphan completeness: no listener owned by a deleted LB may remain")

	rules, err := svc.store.ListRules()
	require.NoError(t, err)
	require.Emptyf(t, rules, "ADR-0002 §5 no-orphan completeness: no rule owned by a deleted LB may remain")
}

// TestRLC3_ELBv2DeleteLBDoesNotBypassListenerCascade enforces ADR-0002 §5 no
// store-bypass cascade: DeleteLoadBalancer must tear listeners (and their
// rules) down through the shared deleteListenerCascade helper, never by
// calling store.DeleteListener directly — a direct store delete orphans the
// listener's rules. The store field is a concrete type with no injection seam,
// so the contract is enforced structurally against the source rather than with
// a runtime spy.
func TestRLC3_ELBv2DeleteLBDoesNotBypassListenerCascade(t *testing.T) {
	src, err := os.ReadFile("service_impl.go")
	require.NoError(t, err)

	body := stripComments(deleteLBFuncBody(t, string(src)))
	assert.Containsf(t, body, "deleteListenerCascade",
		"ADR-0002 §5 no store-bypass cascade: DeleteLoadBalancer must route listener teardown through deleteListenerCascade")
	assert.NotContainsf(t, body, "s.store.DeleteListener(",
		"ADR-0002 §5 no store-bypass cascade: DeleteLoadBalancer must not call store.DeleteListener directly")
}

// deleteLBFuncBody returns the source text of the DeleteLoadBalancer method,
// from its signature up to the start of the next top-level func.
func deleteLBFuncBody(t *testing.T, src string) string {
	t.Helper()
	const sig = "func (s *ELBv2ServiceImpl) DeleteLoadBalancer("
	start := strings.Index(src, sig)
	require.GreaterOrEqual(t, start, 0, "DeleteLoadBalancer not found in service_impl.go")
	rest := src[start+len(sig):]
	if before, _, found := strings.Cut(rest, "\nfunc "); found {
		return before
	}
	return rest
}

// stripComments removes // line comments so source-guard assertions match real
// code, not the prose that documents it.
func stripComments(src string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestRLC4_ELBv2TGDeletableAfterLBTeardown enforces ADR-0002 §5 TG deletability
// after LB teardown: create LB+listener+rule→TG, delete the LB, then the target
// group must delete successfully rather than staying pinned as ResourceInUse by
// an orphaned rule or stale listener default action.
func TestRLC4_ELBv2TGDeletableAfterLBTeardown(t *testing.T) {
	svc := setupTestService(t)

	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("rlc4-lb")}, testAccountID)
	require.NoError(t, err)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("rlc4-tg")}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	lst, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: lst.Listeners[0].ListenerArn,
		Priority:    aws.Int64(10),
		Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/api/*"})}},
		Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: tgArn}, testAccountID)
	require.NoErrorf(t, err, "ADR-0002 §5 TG deletability after LB teardown: target group must not stay pinned as ResourceInUse")
}
