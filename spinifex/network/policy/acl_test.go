package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestACL_TCPPortFromCIDR(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   22,
		ToPort:     22,
		CIDR:       "10.0.0.0/8",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "tcp.dst == 22")
	assert.Contains(t, match, "ip4.src == 10.0.0.0/8")
	assert.Contains(t, match, "outport == @sg_test")
	assert.Contains(t, match, "ip4")
}

func TestACL_AllTrafficFromSG(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		SourceSG:   "sg-abc123",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "ip4.src == $sg_abc123_ip4")
	assert.Contains(t, match, "outport == @sg_test")
	assert.Contains(t, match, "ip4")
}

func TestACL_PortRange(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "udp",
		FromPort:   1024,
		ToPort:     65535,
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "udp.dst >= 1024")
	assert.Contains(t, match, "udp.dst <= 65535")
}

func TestACL_ICMP(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "icmp",
		CIDR:       "0.0.0.0/0",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "icmp4")
	assert.NotContains(t, match, "tcp.dst")
	assert.NotContains(t, match, "udp.dst")
}

func TestACL_AllProtocols(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		CIDR:       "10.0.0.0/16",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "ip4")
	assert.Contains(t, match, "ip4.src == 10.0.0.0/16")
	assert.NotContains(t, match, "tcp")
	assert.NotContains(t, match, "udp")
	assert.NotContains(t, match, "icmp")
}

func TestACL_EgressAll(t *testing.T) {
	match, err := BuildEgressACLMatch("sg_test", Rule{
		IPProtocol: "-1",
		CIDR:       "0.0.0.0/0",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "inport == @sg_test")
	assert.NotContains(t, match, "outport")
	assert.Contains(t, match, "ip4")
}

func TestACL_TCPSinglePort(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   443,
		ToPort:     443,
		CIDR:       "10.0.0.0/8",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "tcp.dst == 443")
	assert.NotContains(t, match, "tcp.dst >=")
	assert.NotContains(t, match, "tcp.dst <=")
}

func TestACL_NoSource(t *testing.T) {
	match, err := BuildIngressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   80,
		ToPort:     80,
		CIDR:       "0.0.0.0/0",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "tcp.dst == 80")
	assert.NotContains(t, match, "ip4.src")
}

func TestACL_EgressFromSGToSG(t *testing.T) {
	match, err := BuildEgressACLMatch("sg_test", Rule{
		IPProtocol: "tcp",
		FromPort:   3306,
		ToPort:     3306,
		SourceSG:   "sg-db-tier",
	})
	assert.NoError(t, err)
	assert.Contains(t, match, "inport == @sg_test")
	assert.Contains(t, match, "tcp.dst == 3306")
	assert.Contains(t, match, "ip4.dst == $sg_db_tier_ip4")
}

// An unrecognised protocol must error, not fall through to a match with no L4
// predicate — that would allow every IP protocol from the source.
func TestACL_UnsupportedProtocolErrors(t *testing.T) {
	for _, proto := range []string{"6", "58", "47", "banana"} {
		_, err := BuildIngressACLMatch("sg_test", Rule{IPProtocol: proto, CIDR: "10.0.0.0/8"})
		assert.Error(t, err, "ingress protocol %q", proto)

		_, err = BuildEgressACLMatch("sg_test", Rule{IPProtocol: proto, CIDR: "10.0.0.0/8"})
		assert.Error(t, err, "egress protocol %q", proto)
	}
}

func TestRuleACLSpecs_UnsupportedProtocolErrors(t *testing.T) {
	_, err := RuleACLSpecs("sg_test", []Rule{{IPProtocol: "6", FromPort: 22, ToPort: 22, CIDR: "10.0.0.0/8"}}, nil)
	assert.Error(t, err)

	_, err = RuleACLSpecs("sg_test", nil, []Rule{{IPProtocol: "6", CIDR: "10.0.0.0/8"}})
	assert.Error(t, err)
}

func TestInfrastructureACLs_Shape(t *testing.T) {
	specs := InfrastructureACLs("sg_test")
	if assert.Len(t, specs, 6) {
		// Priorities: deny-ingress 900, deny-egress 800, dhcp 1050 x2, arp 950 x2
		assert.Equal(t, ACLPriorityDefaultDenyIngress, specs[0].Priority)
		assert.Equal(t, "drop", specs[0].Action)
		assert.True(t, specs[0].Log)
		assert.Equal(t, ACLPriorityDefaultDenyEgress, specs[1].Priority)
		assert.True(t, specs[1].Log)
		assert.Equal(t, ACLPriorityAllowDHCP, specs[2].Priority)
		assert.Equal(t, "allow", specs[2].Action)
		assert.Equal(t, ACLPriorityAllowDHCP, specs[3].Priority)
		assert.Equal(t, ACLPriorityAllowARP, specs[4].Priority)
		assert.Equal(t, "allow", specs[4].Action)
		assert.Equal(t, ACLPriorityAllowARP, specs[5].Priority)
		assert.Equal(t, "allow", specs[5].Action)
	}
}

// The default-denies are the backstop and must match every ethertype. OVN
// allows on no-match, so an ip4-scoped deny lets IPv6 and any other ethertype
// through untouched — the allows are meant to be narrow, the denies are not.
func TestInfrastructureACLs_DenyCarriesNoEthertypeQualifier(t *testing.T) {
	for _, spec := range InfrastructureACLs("sg_test") {
		if spec.Action != "drop" {
			continue
		}
		assert.NotContains(t, spec.Match, "ip4", "deny %q must not be IPv4-scoped", spec.Name)
		assert.NotContains(t, spec.Match, "ip6", "deny %q must not be IPv6-scoped", spec.Name)
	}
}

// ACL tables run before the L2 lookup, so the unqualified denies would drop
// ARP without an explicit allow above them, taking the IPv4 datapath down.
func TestInfrastructureACLs_ARPAllowedAboveDenies(t *testing.T) {
	assert.Greater(t, ACLPriorityAllowARP, ACLPriorityDefaultDenyIngress)
	assert.Less(t, ACLPriorityAllowARP, ACLPriorityTenantAllow)

	byDirection := map[string]string{}
	for _, spec := range InfrastructureACLs("sg_test") {
		if spec.Priority == ACLPriorityAllowARP {
			byDirection[spec.Direction] = spec.Match
		}
	}
	assert.Equal(t, "inport == @sg_test && arp", byDirection["from-lport"])
	assert.Equal(t, "outport == @sg_test && arp", byDirection["to-lport"])
}

// IPv6 is not merely unused: no ENI is assigned a routable IPv6 address, IPv6
// CIDRs are rejected upstream and the derived address sets are _ip4. So ND and
// DHCPv6 stay under the deny rather than becoming an unpoliced side channel.
func TestInfrastructureACLs_NoIPv6Exemption(t *testing.T) {
	for _, spec := range InfrastructureACLs("sg_test") {
		if spec.Action == "drop" {
			continue
		}
		assert.NotContains(t, spec.Match, "nd", "allow %q must not exempt neighbour discovery", spec.Name)
		assert.NotContains(t, spec.Match, "icmp6", "allow %q must not exempt ICMPv6", spec.Name)
	}
}

func TestRuleACLSpecs_PriorityAndAction(t *testing.T) {
	specs, err := RuleACLSpecs("sg_test",
		[]Rule{{IPProtocol: "tcp", FromPort: 80, ToPort: 80, CIDR: "0.0.0.0/0"}},
		[]Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
	)
	assert.NoError(t, err)
	if assert.Len(t, specs, 2) {
		assert.Equal(t, ACLPriorityTenantAllow, specs[0].Priority)
		assert.Equal(t, "to-lport", specs[0].Direction)
		assert.Equal(t, "allow-related", specs[0].Action)
		assert.Equal(t, "from-lport", specs[1].Direction)
		assert.Equal(t, "allow-related", specs[1].Action)
	}
}
