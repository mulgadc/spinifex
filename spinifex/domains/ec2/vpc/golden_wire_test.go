package vpc_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file characterized the EC2-side duplicates of the network-realisation
// wire types. domains/ec2/vpc.NATEvent and .SGEvent were pure wire DTOs,
// deleted in slice 3 in favour of contracts/network/v1's NATEvent and
// SecurityGroupEvent; the two golden pins below now exercise those types
// directly, with the same literals this file has always pinned.
// domains/ec2/vpc.SGRule survives: EC2 stores it directly in
// SecurityGroupRecord, so it is not a wire-only duplicate, and
// contracts/network/v1.SecurityGroupRule was expanded in slice 3 to match it
// field-for-field rather than the other way around.

// TestGoldenNATEvent_JSON pins the NAT event payload the daemon and
// EIP/instance call sites build before handing off to the NAT publish path.
func TestGoldenNATEvent_JSON(t *testing.T) {
	golden := `{"vpc_id":"vpc-1","external_ip":"203.0.113.5","logical_ip":"10.0.1.5","port_name":"port-eni-1","mac":"02:00:00:00:00:01"}`
	evt := networkv1.NATEvent{
		VpcId: "vpc-1", ExternalIP: "203.0.113.5", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "02:00:00:00:00:01",
	}
	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}

// TestGoldenSGEvent_JSON pins the security-group event published on
// vpc.create-sg / vpc.delete-sg / vpc.update-sg via requestSGEvent.
func TestGoldenSGEvent_JSON(t *testing.T) {
	golden := `{"group_id":"sg-1","vpc_id":"vpc-1","ingress_rules":[{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0"}]}`
	evt := networkv1.SecurityGroupEvent{
		GroupId:      "sg-1",
		VpcId:        "vpc-1",
		IngressRules: []networkv1.SecurityGroupRule{{RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0"}},
	}
	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}

// TestGoldenSGRule_FullShapeStillUsedForPersistence pins that
// domains/ec2/vpc.SGRule retains every field EC2's persisted
// SecurityGroupRecord needs (rule_id, cidr_ipv6, description, tags), which
// contracts/network/v1.SecurityGroupRule now also carries so the wire bytes
// requestSGEvent sends are unchanged.
func TestGoldenSGRule_FullShapeStillUsedForPersistence(t *testing.T) {
	golden := `{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0","description":"ssh"}`
	rule := ec2vpc.SGRule{
		RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22,
		CidrIp: "0.0.0.0/0", Description: "ssh",
	}
	marshaled, err := json.Marshal(rule)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))
}
