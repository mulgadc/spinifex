package handlers_elbv2

import (
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
