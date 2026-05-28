package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestACL_TCPPortFromCIDR(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   22,
		ToPort:     22,
		CIDR:       "10.0.0.0/8",
	})
	assert.Contains(t, match, "tcp.dst == 22")
	assert.Contains(t, match, "ip4.src == 10.0.0.0/8")
	assert.Contains(t, match, "outport == @sg_test")
	assert.Contains(t, match, "ip4")
}

func TestACL_AllTrafficFromSG(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		SourceSG:   "sg-abc123",
	})
	assert.Contains(t, match, "ip4.src == $sg_abc123_ip4")
	assert.Contains(t, match, "outport == @sg_test")
	assert.Contains(t, match, "ip4")
}

func TestACL_PortRange(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "udp",
		FromPort:   1024,
		ToPort:     65535,
	})
	assert.Contains(t, match, "udp.dst >= 1024")
	assert.Contains(t, match, "udp.dst <= 65535")
}

func TestACL_ICMP(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "icmp",
		CIDR:       "0.0.0.0/0",
	})
	assert.Contains(t, match, "icmp4")
	assert.NotContains(t, match, "tcp.dst")
	assert.NotContains(t, match, "udp.dst")
}

func TestACL_AllProtocols(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		CIDR:       "10.0.0.0/16",
	})
	assert.Contains(t, match, "ip4")
	assert.Contains(t, match, "ip4.src == 10.0.0.0/16")
	assert.NotContains(t, match, "tcp")
	assert.NotContains(t, match, "udp")
	assert.NotContains(t, match, "icmp")
}

func TestACL_EgressAll(t *testing.T) {
	match := BuildEgressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		CIDR:       "0.0.0.0/0",
	})
	assert.Contains(t, match, "inport == @sg_test")
	assert.NotContains(t, match, "outport")
	assert.Contains(t, match, "ip4")
}

func TestACL_TCPSinglePort(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   443,
		ToPort:     443,
		CIDR:       "10.0.0.0/8",
	})
	assert.Contains(t, match, "tcp.dst == 443")
	assert.NotContains(t, match, "tcp.dst >=")
	assert.NotContains(t, match, "tcp.dst <=")
}

func TestACL_NoSource(t *testing.T) {
	match := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   80,
		ToPort:     80,
		CIDR:       "0.0.0.0/0",
	})
	assert.Contains(t, match, "tcp.dst == 80")
	assert.NotContains(t, match, "ip4.src")
}

func TestACL_EgressFromSGToSG(t *testing.T) {
	match := BuildEgressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   3306,
		ToPort:     3306,
		SourceSG:   "sg-db-tier",
	})
	assert.Contains(t, match, "inport == @sg_test")
	assert.Contains(t, match, "tcp.dst == 3306")
	assert.Contains(t, match, "ip4.dst == $sg_db_tier_ip4")
}

func TestInfrastructureACLs_Shape(t *testing.T) {
	specs := InfrastructureACLs("sg_test")
	if assert.Len(t, specs, 4) {
		// Priorities: deny-ingress 900, deny-egress 800, dhcp-egress 1050, dhcp-ingress 1050
		assert.Equal(t, ACLPriorityDefaultDenyIngress, specs[0].Priority)
		assert.Equal(t, "drop", specs[0].Action)
		assert.True(t, specs[0].Log)
		assert.Equal(t, ACLPriorityDefaultDenyEgress, specs[1].Priority)
		assert.True(t, specs[1].Log)
		assert.Equal(t, ACLPriorityAllowDHCP, specs[2].Priority)
		assert.Equal(t, "allow", specs[2].Action)
		assert.Equal(t, ACLPriorityAllowDHCP, specs[3].Priority)
	}
}

func TestRuleACLSpecs_PriorityAndAction(t *testing.T) {
	specs := RuleACLSpecs("sg_test",
		[]Rule{{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"}},
		[]Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
	)
	if assert.Len(t, specs, 2) {
		assert.Equal(t, ACLPriorityTenantAllow, specs[0].Priority)
		assert.Equal(t, "to-lport", specs[0].Direction)
		assert.Equal(t, "allow-related", specs[0].Action)
		assert.Equal(t, "from-lport", specs[1].Direction)
		assert.Equal(t, "allow-related", specs[1].Action)
	}
}
