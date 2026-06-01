package handlers_elbv2

import (
	"time"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	defaultTimeout = 30 * time.Second
	// longRunningTimeout covers operations that synchronously launch or tear
	// down ALB frontend VMs (DHCP lease + QEMU boot / QMP shutdown + QEMU exit).
	// A healthy launch is ~45s on a warm box; shutdown can take tens of seconds
	// if QMP is slow. Until CreateLoadBalancer is made fully async, we keep a
	// wide margin so Terraform's AWS provider doesn't retry mid-flight and then
	// hit DuplicateLoadBalancerName on the second attempt.
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

func (s *NATSELBv2Service) CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	return utils.NATSRequest[elbv2.CreateLoadBalancerOutput](s.natsConn, "elbv2.CreateLoadBalancer", input, longRunningTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error) {
	return utils.NATSRequest[elbv2.DeleteLoadBalancerOutput](s.natsConn, "elbv2.DeleteLoadBalancer", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeLoadBalancers(input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error) {
	return utils.NATSRequest[elbv2.DescribeLoadBalancersOutput](s.natsConn, "elbv2.DescribeLoadBalancers", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateTargetGroup(input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error) {
	return utils.NATSRequest[elbv2.CreateTargetGroupOutput](s.natsConn, "elbv2.CreateTargetGroup", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteTargetGroup(input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error) {
	return utils.NATSRequest[elbv2.DeleteTargetGroupOutput](s.natsConn, "elbv2.DeleteTargetGroup", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error) {
	return utils.NATSRequest[elbv2.DescribeTargetGroupsOutput](s.natsConn, "elbv2.DescribeTargetGroups", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) RegisterTargets(input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error) {
	return utils.NATSRequest[elbv2.RegisterTargetsOutput](s.natsConn, "elbv2.RegisterTargets", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeregisterTargets(input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error) {
	return utils.NATSRequest[elbv2.DeregisterTargetsOutput](s.natsConn, "elbv2.DeregisterTargets", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetHealth(input *elbv2.DescribeTargetHealthInput, accountID string) (*elbv2.DescribeTargetHealthOutput, error) {
	return utils.NATSRequest[elbv2.DescribeTargetHealthOutput](s.natsConn, "elbv2.DescribeTargetHealth", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateListener(input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error) {
	return utils.NATSRequest[elbv2.CreateListenerOutput](s.natsConn, "elbv2.CreateListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteListener(input *elbv2.DeleteListenerInput, accountID string) (*elbv2.DeleteListenerOutput, error) {
	return utils.NATSRequest[elbv2.DeleteListenerOutput](s.natsConn, "elbv2.DeleteListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyListener(input *elbv2.ModifyListenerInput, accountID string) (*elbv2.ModifyListenerOutput, error) {
	return utils.NATSRequest[elbv2.ModifyListenerOutput](s.natsConn, "elbv2.ModifyListener", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeListeners(input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error) {
	return utils.NATSRequest[elbv2.DescribeListenersOutput](s.natsConn, "elbv2.DescribeListeners", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) CreateRule(input *elbv2.CreateRuleInput, accountID string) (*elbv2.CreateRuleOutput, error) {
	return utils.NATSRequest[elbv2.CreateRuleOutput](s.natsConn, "elbv2.CreateRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyRule(input *elbv2.ModifyRuleInput, accountID string) (*elbv2.ModifyRuleOutput, error) {
	return utils.NATSRequest[elbv2.ModifyRuleOutput](s.natsConn, "elbv2.ModifyRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DeleteRule(input *elbv2.DeleteRuleInput, accountID string) (*elbv2.DeleteRuleOutput, error) {
	return utils.NATSRequest[elbv2.DeleteRuleOutput](s.natsConn, "elbv2.DeleteRule", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeRules(input *elbv2.DescribeRulesInput, accountID string) (*elbv2.DescribeRulesOutput, error) {
	return utils.NATSRequest[elbv2.DescribeRulesOutput](s.natsConn, "elbv2.DescribeRules", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) SetRulePriorities(input *elbv2.SetRulePrioritiesInput, accountID string) (*elbv2.SetRulePrioritiesOutput, error) {
	return utils.NATSRequest[elbv2.SetRulePrioritiesOutput](s.natsConn, "elbv2.SetRulePriorities", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTags(input *elbv2.DescribeTagsInput, accountID string) (*elbv2.DescribeTagsOutput, error) {
	return utils.NATSRequest[elbv2.DescribeTagsOutput](s.natsConn, "elbv2.DescribeTags", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyTargetGroupAttributes(input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
	return utils.NATSRequest[elbv2.ModifyTargetGroupAttributesOutput](s.natsConn, "elbv2.ModifyTargetGroupAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeTargetGroupAttributes(input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
	return utils.NATSRequest[elbv2.DescribeTargetGroupAttributesOutput](s.natsConn, "elbv2.DescribeTargetGroupAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) ModifyLoadBalancerAttributes(input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
	return utils.NATSRequest[elbv2.ModifyLoadBalancerAttributesOutput](s.natsConn, "elbv2.ModifyLoadBalancerAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) DescribeLoadBalancerAttributes(input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
	return utils.NATSRequest[elbv2.DescribeLoadBalancerAttributesOutput](s.natsConn, "elbv2.DescribeLoadBalancerAttributes", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) LBAgentHeartbeat(input *LBAgentHeartbeatInput, accountID string) (*LBAgentHeartbeatOutput, error) {
	return utils.NATSRequest[LBAgentHeartbeatOutput](s.natsConn, "elbv2.LBAgentHeartbeat", input, defaultTimeout, accountID)
}

func (s *NATSELBv2Service) GetLBConfig(input *GetLBConfigInput, accountID string) (*GetLBConfigOutput, error) {
	return utils.NATSRequest[GetLBConfigOutput](s.natsConn, "elbv2.GetLBConfig", input, defaultTimeout, accountID)
}
