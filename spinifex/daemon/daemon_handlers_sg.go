package daemon

import "github.com/nats-io/nats.go"

func (d *Daemon) handleEC2CreateSecurityGroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.CreateSecurityGroup)
}

func (d *Daemon) handleEC2DeleteSecurityGroup(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DeleteSecurityGroup)
}

func (d *Daemon) handleEC2DescribeSecurityGroups(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeSecurityGroups)
}

func (d *Daemon) handleEC2DescribeSecurityGroupRules(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.DescribeSecurityGroupRules)
}

func (d *Daemon) handleEC2AuthorizeSecurityGroupIngress(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.AuthorizeSecurityGroupIngress)
}

func (d *Daemon) handleEC2AuthorizeSecurityGroupEgress(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.AuthorizeSecurityGroupEgress)
}

func (d *Daemon) handleEC2RevokeSecurityGroupIngress(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.RevokeSecurityGroupIngress)
}

func (d *Daemon) handleEC2RevokeSecurityGroupEgress(msg *nats.Msg) {
	handleNATSRequest(msg, d.vpcService.RevokeSecurityGroupEgress)
}
