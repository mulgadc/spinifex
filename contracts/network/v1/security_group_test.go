package networkv1_test

import (
	"encoding/json"
	"testing"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecurityGroupSubjects(t *testing.T) {
	assert.Equal(t, "vpc.create-sg", networkv1.SecurityGroupCreateSubject)
	assert.Equal(t, "vpc.delete-sg", networkv1.SecurityGroupDeleteSubject)
	assert.Equal(t, "vpc.update-sg", networkv1.SecurityGroupUpdateSubject)
}

func TestSecurityGroupEvent_JSON(t *testing.T) {
	golden := `{"group_id":"sg-1","vpc_id":"vpc-1","ingress_rules":[{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0"}],"egress_rules":[{"rule_id":"sgr-2","ip_protocol":"-1","from_port":0,"to_port":0,"source_sg":"sg-2"}]}`
	evt := networkv1.SecurityGroupEvent{
		GroupId: "sg-1", VpcId: "vpc-1",
		IngressRules: []networkv1.SecurityGroupRule{{RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0"}},
		EgressRules:  []networkv1.SecurityGroupRule{{RuleId: "sgr-2", IpProtocol: "-1", SourceSG: "sg-2"}},
	}

	marshaled, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded networkv1.SecurityGroupEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, evt, decoded)
}

// TestSecurityGroupRule_FullShapeRoundTrip pins the EC2 publisher's stored
// rule shape (rule_id, cidr_ipv6, description, tags) round-tripping losslessly;
// network's ACL builder still only reads the first five fields.
func TestSecurityGroupRule_FullShapeRoundTrip(t *testing.T) {
	wider := `{"rule_id":"sgr-1","ip_protocol":"tcp","from_port":22,"to_port":22,"cidr_ip":"0.0.0.0/0","cidr_ipv6":"::/0","description":"ssh","tags":{"k":"v"}}`
	var decoded networkv1.SecurityGroupRule
	require.NoError(t, json.Unmarshal([]byte(wider), &decoded))
	assert.Equal(t, networkv1.SecurityGroupRule{
		RuleId: "sgr-1", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0",
		CidrIpv6: "::/0", Description: "ssh", Tags: map[string]string{"k": "v"},
	}, decoded)

	marshaled, err := json.Marshal(decoded)
	require.NoError(t, err)
	assert.JSONEq(t, wider, string(marshaled))
}
