package handlers_elbv2

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	defaultTimeout = 30 * time.Second
	// longRunningTimeout covers synchronous ALB VM launch/teardown (~45 s on a warm
	// box). Wide margin prevents Terraform retrying mid-flight and hitting
	// DuplicateLoadBalancerName before CreateLoadBalancer is made async.
	longRunningTimeout = 5 * time.Minute
)

// NATSELBv2Service handles ELBv2 operations via NATS messaging.
type NATSELBv2Service struct {
	natsConn *nats.Conn
}

var _ ELBv2Service = (*NATSELBv2Service)(nil)

// NewNATSELBv2Service creates a new NATS-based ELBv2 service.
func NewNATSELBv2Service(conn *nats.Conn) ELBv2Service {
	return &NATSELBv2Service{natsConn: conn}
}

func (s *NATSELBv2Service) CreateLoadBalancer(ctx context.Context, input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	return utils.NatsRequest[elbv2.CreateLoadBalancerOutput](ctx, s.natsConn, "elbv2.CreateLoadBalancer", input, longRunningTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteLoadBalancer(ctx context.Context, input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error) {
	return utils.NatsRequest[elbv2.DeleteLoadBalancerOutput](ctx, s.natsConn, "elbv2.DeleteLoadBalancer", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeLoadBalancers(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error) {
	return utils.NatsRequest[elbv2.DescribeLoadBalancersOutput](ctx, s.natsConn, "elbv2.DescribeLoadBalancers", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateTargetGroup(ctx context.Context, input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error) {
	return utils.NatsRequest[elbv2.CreateTargetGroupOutput](ctx, s.natsConn, "elbv2.CreateTargetGroup", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyTargetGroup(ctx context.Context, input *elbv2.ModifyTargetGroupInput, accountID string) (*elbv2.ModifyTargetGroupOutput, error) {
	return utils.NatsRequest[elbv2.ModifyTargetGroupOutput](ctx, s.natsConn, "elbv2.ModifyTargetGroup", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteTargetGroup(ctx context.Context, input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error) {
	return utils.NatsRequest[elbv2.DeleteTargetGroupOutput](ctx, s.natsConn, "elbv2.DeleteTargetGroup", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetGroups(ctx context.Context, input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error) {
	return utils.NatsRequest[elbv2.DescribeTargetGroupsOutput](ctx, s.natsConn, "elbv2.DescribeTargetGroups", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) RegisterTargets(ctx context.Context, input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error) {
	return utils.NatsRequest[elbv2.RegisterTargetsOutput](ctx, s.natsConn, "elbv2.RegisterTargets", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeregisterTargets(ctx context.Context, input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error) {
	return utils.NatsRequest[elbv2.DeregisterTargetsOutput](ctx, s.natsConn, "elbv2.DeregisterTargets", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetHealth(ctx context.Context, input *elbv2.DescribeTargetHealthInput, accountID string) (*elbv2.DescribeTargetHealthOutput, error) {
	return utils.NatsRequest[elbv2.DescribeTargetHealthOutput](ctx, s.natsConn, "elbv2.DescribeTargetHealth", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateListener(ctx context.Context, input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error) {
	return utils.NatsRequest[elbv2.CreateListenerOutput](ctx, s.natsConn, "elbv2.CreateListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteListener(ctx context.Context, input *elbv2.DeleteListenerInput, accountID string) (*elbv2.DeleteListenerOutput, error) {
	return utils.NatsRequest[elbv2.DeleteListenerOutput](ctx, s.natsConn, "elbv2.DeleteListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyListener(ctx context.Context, input *elbv2.ModifyListenerInput, accountID string) (*elbv2.ModifyListenerOutput, error) {
	return utils.NatsRequest[elbv2.ModifyListenerOutput](ctx, s.natsConn, "elbv2.ModifyListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeListeners(ctx context.Context, input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error) {
	return utils.NatsRequest[elbv2.DescribeListenersOutput](ctx, s.natsConn, "elbv2.DescribeListeners", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateRule(ctx context.Context, input *elbv2.CreateRuleInput, accountID string) (*elbv2.CreateRuleOutput, error) {
	return utils.NatsRequest[elbv2.CreateRuleOutput](ctx, s.natsConn, "elbv2.CreateRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyRule(ctx context.Context, input *elbv2.ModifyRuleInput, accountID string) (*elbv2.ModifyRuleOutput, error) {
	return utils.NatsRequest[elbv2.ModifyRuleOutput](ctx, s.natsConn, "elbv2.ModifyRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteRule(ctx context.Context, input *elbv2.DeleteRuleInput, accountID string) (*elbv2.DeleteRuleOutput, error) {
	return utils.NatsRequest[elbv2.DeleteRuleOutput](ctx, s.natsConn, "elbv2.DeleteRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeRules(ctx context.Context, input *elbv2.DescribeRulesInput, accountID string) (*elbv2.DescribeRulesOutput, error) {
	return utils.NatsRequest[elbv2.DescribeRulesOutput](ctx, s.natsConn, "elbv2.DescribeRules", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) SetRulePriorities(ctx context.Context, input *elbv2.SetRulePrioritiesInput, accountID string) (*elbv2.SetRulePrioritiesOutput, error) {
	return utils.NatsRequest[elbv2.SetRulePrioritiesOutput](ctx, s.natsConn, "elbv2.SetRulePriorities", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTags(ctx context.Context, input *elbv2.DescribeTagsInput, accountID string) (*elbv2.DescribeTagsOutput, error) {
	return utils.NatsRequest[elbv2.DescribeTagsOutput](ctx, s.natsConn, "elbv2.DescribeTags", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) AddTags(ctx context.Context, input *elbv2.AddTagsInput, accountID string) (*elbv2.AddTagsOutput, error) {
	return utils.NatsRequest[elbv2.AddTagsOutput](ctx, s.natsConn, "elbv2.AddTags", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) RemoveTags(ctx context.Context, input *elbv2.RemoveTagsInput, accountID string) (*elbv2.RemoveTagsOutput, error) {
	return utils.NatsRequest[elbv2.RemoveTagsOutput](ctx, s.natsConn, "elbv2.RemoveTags", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyTargetGroupAttributes(ctx context.Context, input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
	return utils.NatsRequest[elbv2.ModifyTargetGroupAttributesOutput](ctx, s.natsConn, "elbv2.ModifyTargetGroupAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetGroupAttributes(ctx context.Context, input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
	return utils.NatsRequest[elbv2.DescribeTargetGroupAttributesOutput](ctx, s.natsConn, "elbv2.DescribeTargetGroupAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyLoadBalancerAttributes(ctx context.Context, input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
	return utils.NatsRequest[elbv2.ModifyLoadBalancerAttributesOutput](ctx, s.natsConn, "elbv2.ModifyLoadBalancerAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeLoadBalancerAttributes(ctx context.Context, input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
	return utils.NatsRequest[elbv2.DescribeLoadBalancerAttributesOutput](ctx, s.natsConn, "elbv2.DescribeLoadBalancerAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) SetSecurityGroups(ctx context.Context, input *elbv2.SetSecurityGroupsInput, accountID string) (*elbv2.SetSecurityGroupsOutput, error) {
	return utils.NatsRequest[elbv2.SetSecurityGroupsOutput](ctx, s.natsConn, "elbv2.SetSecurityGroups", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) SetIpAddressType(ctx context.Context, input *elbv2.SetIpAddressTypeInput, accountID string) (*elbv2.SetIpAddressTypeOutput, error) {
	return utils.NatsRequest[elbv2.SetIpAddressTypeOutput](ctx, s.natsConn, "elbv2.SetIpAddressType", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) SetSubnets(ctx context.Context, input *elbv2.SetSubnetsInput, accountID string) (*elbv2.SetSubnetsOutput, error) {
	return utils.NatsRequest[elbv2.SetSubnetsOutput](ctx, s.natsConn, "elbv2.SetSubnets", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) AddListenerCertificates(ctx context.Context, input *elbv2.AddListenerCertificatesInput, accountID string) (*elbv2.AddListenerCertificatesOutput, error) {
	return utils.NatsRequest[elbv2.AddListenerCertificatesOutput](ctx, s.natsConn, "elbv2.AddListenerCertificates", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) RemoveListenerCertificates(ctx context.Context, input *elbv2.RemoveListenerCertificatesInput, accountID string) (*elbv2.RemoveListenerCertificatesOutput, error) {
	return utils.NatsRequest[elbv2.RemoveListenerCertificatesOutput](ctx, s.natsConn, "elbv2.RemoveListenerCertificates", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeListenerCertificates(ctx context.Context, input *elbv2.DescribeListenerCertificatesInput, accountID string) (*elbv2.DescribeListenerCertificatesOutput, error) {
	return utils.NatsRequest[elbv2.DescribeListenerCertificatesOutput](ctx, s.natsConn, "elbv2.DescribeListenerCertificates", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeSSLPolicies(ctx context.Context, input *elbv2.DescribeSSLPoliciesInput, accountID string) (*elbv2.DescribeSSLPoliciesOutput, error) {
	return utils.NatsRequest[elbv2.DescribeSSLPoliciesOutput](ctx, s.natsConn, "elbv2.DescribeSSLPolicies", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) LBAgentHeartbeat(ctx context.Context, input *LBAgentHeartbeatInput, accountID string) (*LBAgentHeartbeatOutput, error) {
	return utils.NatsRequest[LBAgentHeartbeatOutput](ctx, s.natsConn, "elbv2.LBAgentHeartbeat", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) GetLBConfig(ctx context.Context, input *GetLBConfigInput, accountID string) (*GetLBConfigOutput, error) {
	return utils.NatsRequest[GetLBConfigOutput](ctx, s.natsConn, "elbv2.GetLBConfig", input, defaultTimeout, accountID)
}
