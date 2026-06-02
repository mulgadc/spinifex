package handlers_elbv2

import "github.com/aws/aws-sdk-go/service/elbv2"

// ELBv2Service defines the interface for Application Load Balancer operations.
type ELBv2Service interface {
	CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error)
	DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error)
	DescribeLoadBalancers(input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error)
	ModifyLoadBalancerAttributes(input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error)
	DescribeLoadBalancerAttributes(input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error)

	CreateTargetGroup(input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error)
	ModifyTargetGroup(input *elbv2.ModifyTargetGroupInput, accountID string) (*elbv2.ModifyTargetGroupOutput, error)
	DeleteTargetGroup(input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error)
	DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error)
	ModifyTargetGroupAttributes(input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error)
	DescribeTargetGroupAttributes(input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error)

	RegisterTargets(input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error)
	DeregisterTargets(input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error)
	DescribeTargetHealth(input *elbv2.DescribeTargetHealthInput, accountID string) (*elbv2.DescribeTargetHealthOutput, error)

	CreateListener(input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error)
	DeleteListener(input *elbv2.DeleteListenerInput, accountID string) (*elbv2.DeleteListenerOutput, error)
	ModifyListener(input *elbv2.ModifyListenerInput, accountID string) (*elbv2.ModifyListenerOutput, error)
	DescribeListeners(input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error)

	CreateRule(input *elbv2.CreateRuleInput, accountID string) (*elbv2.CreateRuleOutput, error)
	ModifyRule(input *elbv2.ModifyRuleInput, accountID string) (*elbv2.ModifyRuleOutput, error)
	DeleteRule(input *elbv2.DeleteRuleInput, accountID string) (*elbv2.DeleteRuleOutput, error)
	DescribeRules(input *elbv2.DescribeRulesInput, accountID string) (*elbv2.DescribeRulesOutput, error)
	SetRulePriorities(input *elbv2.SetRulePrioritiesInput, accountID string) (*elbv2.SetRulePrioritiesOutput, error)

	DescribeTags(input *elbv2.DescribeTagsInput, accountID string) (*elbv2.DescribeTagsOutput, error)

	LBAgentHeartbeat(input *LBAgentHeartbeatInput, accountID string) (*LBAgentHeartbeatOutput, error)
	GetLBConfig(input *GetLBConfigInput, accountID string) (*GetLBConfigOutput, error)
}
