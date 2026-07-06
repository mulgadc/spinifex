package handlers_elbv2

import (
	"context"

	"github.com/aws/aws-sdk-go/service/elbv2"
)

// ELBv2Service defines the interface for Application Load Balancer operations.
type ELBv2Service interface {
	CreateLoadBalancer(ctx context.Context, input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error)
	DeleteLoadBalancer(ctx context.Context, input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error)
	DescribeLoadBalancers(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error)
	ModifyLoadBalancerAttributes(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error)
	DescribeLoadBalancerAttributes(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error)
	SetSecurityGroups(ctx context.Context, input *elbv2.SetSecurityGroupsInput, accountID string) (*elbv2.SetSecurityGroupsOutput, error)
	SetIpAddressType(ctx context.Context, input *elbv2.SetIpAddressTypeInput, accountID string) (*elbv2.SetIpAddressTypeOutput, error)
	SetSubnets(ctx context.Context, input *elbv2.SetSubnetsInput, accountID string) (*elbv2.SetSubnetsOutput, error)

	CreateTargetGroup(ctx context.Context, input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error)
	ModifyTargetGroup(ctx context.Context, input *elbv2.ModifyTargetGroupInput, accountID string) (*elbv2.ModifyTargetGroupOutput, error)
	DeleteTargetGroup(ctx context.Context, input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error)
	DescribeTargetGroups(ctx context.Context, input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error)
	ModifyTargetGroupAttributes(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error)
	DescribeTargetGroupAttributes(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error)

	RegisterTargets(ctx context.Context, input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error)
	DeregisterTargets(ctx context.Context, input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error)
	DescribeTargetHealth(ctx context.Context, input *elbv2.DescribeTargetHealthInput, accountID string) (*elbv2.DescribeTargetHealthOutput, error)

	CreateListener(ctx context.Context, input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error)
	DeleteListener(ctx context.Context, input *elbv2.DeleteListenerInput, accountID string) (*elbv2.DeleteListenerOutput, error)
	ModifyListener(ctx context.Context, input *elbv2.ModifyListenerInput, accountID string) (*elbv2.ModifyListenerOutput, error)
	DescribeListeners(ctx context.Context, input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error)

	AddListenerCertificates(ctx context.Context, input *elbv2.AddListenerCertificatesInput, accountID string) (*elbv2.AddListenerCertificatesOutput, error)
	RemoveListenerCertificates(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, accountID string) (*elbv2.RemoveListenerCertificatesOutput, error)
	DescribeListenerCertificates(ctx context.Context, input *elbv2.DescribeListenerCertificatesInput, accountID string) (*elbv2.DescribeListenerCertificatesOutput, error)
	DescribeSSLPolicies(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, accountID string) (*elbv2.DescribeSSLPoliciesOutput, error)

	CreateRule(ctx context.Context, input *elbv2.CreateRuleInput, accountID string) (*elbv2.CreateRuleOutput, error)
	ModifyRule(ctx context.Context, input *elbv2.ModifyRuleInput, accountID string) (*elbv2.ModifyRuleOutput, error)
	DeleteRule(ctx context.Context, input *elbv2.DeleteRuleInput, accountID string) (*elbv2.DeleteRuleOutput, error)
	DescribeRules(ctx context.Context, input *elbv2.DescribeRulesInput, accountID string) (*elbv2.DescribeRulesOutput, error)
	SetRulePriorities(ctx context.Context, input *elbv2.SetRulePrioritiesInput, accountID string) (*elbv2.SetRulePrioritiesOutput, error)

	DescribeTags(ctx context.Context, input *elbv2.DescribeTagsInput, accountID string) (*elbv2.DescribeTagsOutput, error)
	AddTags(ctx context.Context, input *elbv2.AddTagsInput, accountID string) (*elbv2.AddTagsOutput, error)
	RemoveTags(ctx context.Context, input *elbv2.RemoveTagsInput, accountID string) (*elbv2.RemoveTagsOutput, error)

	LBAgentHeartbeat(ctx context.Context, input *LBAgentHeartbeatInput, accountID string) (*LBAgentHeartbeatOutput, error)
	GetLBConfig(ctx context.Context, input *GetLBConfigInput, accountID string) (*GetLBConfigOutput, error)
}
