package vpc_test

import (
	"encoding/json"
	"testing"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file characterizes the EC2-side duplicates of the network-realisation
// wire types: domains/ec2/vpc.NATEvent and .SGEvent/.SGRule are independent
// Go type declarations from subscribers.NATEvent/SGEvent/SGRule, kept in sync
// by convention rather than a shared package. contracts/network/v1 (slice 2)
// must reproduce the subset both sides agree on.

// TestGoldenNATEvent_JSON pins domains/ec2/vpc.NATEvent, the type the daemon
// and EIP/instance call sites build before handing off to utils.PublishNATEvent.
func TestGoldenNATEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
	evt := ec2vpc.NATEvent{
		VpcId: "vpc-1", ExternalIP: "203.0.113.5", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "02:00:00:00:00:01",
	}
	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}

// TestGoldenSGRule_HasFieldsSubscriberSGRuleDoesNot pins a real asymmetry: the
// publisher-side SGRule carries rule_id, cidr_ipv6, description and tags: the
// consumer-side subscribers.SGRule (events.go) declares only ip_protocol,
// from_port, to_port, cidr_ip and source_sg. json.Unmarshal silently drops
// unknown keys, so this is forward-compatible today, but a contract that
// copies only the 5-field shape must document that the other 4 are dropped,
// not forward it as a partial projection.
func TestGoldenSGRule_HasFieldsSubscriberSGRuleDoesNot(t *testing.T) {
	golden := `{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0","description":"ssh"}`
	rule := ec2vpc.SGRule{
		RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22,
		CidrIp: "0.0.0.0/0", Description: "ssh",
	}
	marshaled, err := json.Marshal(rule)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}

// TestGoldenSGEvent_JSON pins domains/ec2/vpc.SGEvent, published on
// vpc.create-sg / vpc.delete-sg / vpc.update-sg via requestSGEvent.
func TestGoldenSGEvent_JSON(t *testing.T) {
	golden := `{"group_id":"sg-1","vpc_id":"vpc-1","ingress_rules":[{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0"}]}`
	evt := ec2vpc.SGEvent{
		GroupId: "sg-1", VpcId: "vpc-1",
		IngressRules: []ec2vpc.SGRule{{RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0"}},
	}
	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}
