package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleELBv2CreateLoadBalancer(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.CreateLoadBalancer)
}

func (d *Daemon) handleELBv2DeleteLoadBalancer(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DeleteLoadBalancer)
}

func (d *Daemon) handleELBv2DescribeLoadBalancers(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeLoadBalancers)
}

func (d *Daemon) handleELBv2CreateTargetGroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.CreateTargetGroup)
}

func (d *Daemon) handleELBv2DeleteTargetGroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DeleteTargetGroup)
}

func (d *Daemon) handleELBv2DescribeTargetGroups(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeTargetGroups)
}

func (d *Daemon) handleELBv2RegisterTargets(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.RegisterTargets)
}

func (d *Daemon) handleELBv2DeregisterTargets(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DeregisterTargets)
}

func (d *Daemon) handleELBv2DescribeTargetHealth(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeTargetHealth)
}

func (d *Daemon) handleELBv2CreateListener(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.CreateListener)
}

func (d *Daemon) handleELBv2DeleteListener(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DeleteListener)
}

func (d *Daemon) handleELBv2ModifyListener(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.ModifyListener)
}

func (d *Daemon) handleELBv2DescribeListeners(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeListeners)
}

func (d *Daemon) handleELBv2DescribeTags(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeTags)
}

func (d *Daemon) handleELBv2LBAgentHeartbeat(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.LBAgentHeartbeat)
}

func (d *Daemon) handleELBv2GetLBConfig(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.GetLBConfig)
}

func (d *Daemon) handleELBv2ModifyTargetGroupAttributes(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.ModifyTargetGroupAttributes)
}

func (d *Daemon) handleELBv2DescribeTargetGroupAttributes(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeTargetGroupAttributes)
}

func (d *Daemon) handleELBv2ModifyLoadBalancerAttributes(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.ModifyLoadBalancerAttributes)
}

func (d *Daemon) handleELBv2DescribeLoadBalancerAttributes(msg *nats.Msg) {
	handleNATSRequest(msg, d.elbv2Service.DescribeLoadBalancerAttributes)
}
