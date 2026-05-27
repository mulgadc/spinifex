package policy

import (
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// ACL priority bands. Higher wins. Tenants only ever populate 1000.
const (
	// ACLPriorityAllowIntraSG: unconditional intra-PG allow (reserved).
	ACLPriorityAllowIntraSG = 1100

	// ACLPriorityAllowDHCP must sit above tenant rules so a narrow-egress SG
	// doesn't drop DHCPDISCOVER against the 900 default-deny. AWS users
	// can't write a 255.255.255.255 rule.
	ACLPriorityAllowDHCP = 1050

	// ACLPriorityTenantAllow: tenant ingress/egress allows. Not logged.
	ACLPriorityTenantAllow = 1000

	// ACLPriorityDefaultDenyIngress: logged drop. CMMC SC.L1-3.13.1.
	ACLPriorityDefaultDenyIngress = 900

	// ACLPriorityDefaultDenyEgress: egress default-deny.
	ACLPriorityDefaultDenyEgress = 800
)

// denyACLSeverity: syslog severity for default-deny hits. "info" captures
// without paging on port scans; operators can promote at the collector.
const denyACLSeverity = "info"

// Rule is the policy-layer view of an AWS-style SG rule. CIDR / SourceSG
// MUST be validated upstream; values are interpolated verbatim.
type Rule struct {
	IPProtocol string // "tcp", "udp", "icmp", or "-1" (all)
	FromPort   int64
	ToPort     int64
	CIDR       string
	SourceSG   string
}

// InfrastructureACLs returns the platform ACLs every PG carries: the
// 900/800 logged default-denies (CMMC SC.L1-3.13.1) and the 1050 DHCPv4
// client/server allows (OVN's DHCP responder runs after the PG ACL stage;
// without these a narrow-egress SG would drop DHCPDISCOVER).
func InfrastructureACLs(portGroupName string) []ovn.ACLSpec {
	return []ovn.ACLSpec{
		denyIngressACL(portGroupName),
		denyEgressACL(portGroupName),
		dhcpEgressACL(portGroupName),
		dhcpIngressACL(portGroupName),
	}
}

// RuleACLSpecs builds priority-1000 allow ACLs. Action is "allow-related"
// so reply traffic on established flows passes without a reverse rule.
func RuleACLSpecs(portGroupName string, ingress, egress []Rule) []ovn.ACLSpec {
	specs := make([]ovn.ACLSpec, 0, len(ingress)+len(egress))
	for _, rule := range ingress {
		specs = append(specs, ovn.ACLSpec{
			Direction: "to-lport",
			Priority:  ACLPriorityTenantAllow,
			Match:     BuildIngressACLMatch(portGroupName, rule),
			Action:    "allow-related",
		})
	}
	for _, rule := range egress {
		specs = append(specs, ovn.ACLSpec{
			Direction: "from-lport",
			Priority:  ACLPriorityTenantAllow,
			Match:     BuildEgressACLMatch(portGroupName, rule),
			Action:    "allow-related",
		})
	}
	return specs
}

// BuildIngressACLMatch builds an OVN to-lport match. Tenant CIDR/SourceSG
// MUST be validated upstream; values are interpolated verbatim.
func BuildIngressACLMatch(portGroupName string, rule Rule) string {
	parts := []string{fmt.Sprintf("outport == @%s", portGroupName), "ip4"}
	parts = appendProtocolMatch(parts, rule)
	if rule.CIDR != "" && rule.CIDR != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.src == %s", rule.CIDR))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.src == $%s", addressSetName(topology.SecurityGroupPortGroup(rule.SourceSG))))
	}
	return strings.Join(parts, " && ")
}

// BuildEgressACLMatch builds an OVN from-lport match.
func BuildEgressACLMatch(portGroupName string, rule Rule) string {
	parts := []string{fmt.Sprintf("inport == @%s", portGroupName), "ip4"}
	parts = appendProtocolMatch(parts, rule)
	if rule.CIDR != "" && rule.CIDR != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.dst == %s", rule.CIDR))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.dst == $%s", addressSetName(topology.SecurityGroupPortGroup(rule.SourceSG))))
	}
	return strings.Join(parts, " && ")
}

// addressSetName returns the ovn-northd-derived SB Address_Set name for a
// PG's IPv4 addresses. Spinifex must NOT create NB Address_Set rows under
// these names — duplicate SB sync wedges northd in a commit-failed loop.
func addressSetName(portGroupName string) string {
	return portGroupName + "_ip4"
}

func appendProtocolMatch(parts []string, rule Rule) []string {
	switch rule.IPProtocol {
	case "tcp":
		parts = appendPortMatch(parts, "tcp", rule.FromPort, rule.ToPort)
	case "udp":
		parts = appendPortMatch(parts, "udp", rule.FromPort, rule.ToPort)
	case "icmp":
		parts = append(parts, "icmp4")
	case "-1", "":
	default:
	}
	return parts
}

func appendPortMatch(parts []string, proto string, fromPort, toPort int64) []string {
	if fromPort == 0 && toPort == 0 {
		return append(parts, proto)
	}
	if fromPort == toPort {
		return append(parts, fmt.Sprintf("%s.dst == %d", proto, fromPort))
	}
	parts = append(parts, fmt.Sprintf("%s.dst >= %d", proto, fromPort))
	parts = append(parts, fmt.Sprintf("%s.dst <= %d", proto, toPort))
	return parts
}

func denyIngressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "to-lport",
		Priority:  ACLPriorityDefaultDenyIngress,
		Match:     fmt.Sprintf("outport == @%s && ip4", portGroupName),
		Action:    "drop",
		Name:      portGroupName + "-deny-ingress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

func denyEgressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityDefaultDenyEgress,
		Match:     fmt.Sprintf("inport == @%s && ip4", portGroupName),
		Action:    "drop",
		Name:      portGroupName + "-deny-egress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

// dhcpEgressACL: DHCPDISCOVER/REQUEST out (udp 68→67). "allow" not
// "allow-related" — broadcast UDP interacts oddly with OVN CT zones.
func dhcpEgressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityAllowDHCP,
		Match:     fmt.Sprintf("inport == @%s && udp && udp.src == 68 && udp.dst == 67", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-dhcp-egress",
	}
}

// dhcpIngressACL: DHCPOFFER/ACK reply (udp 67→68). Required because OVN's
// DHCP responder emits inside the LS pipeline after the ACL stage.
func dhcpIngressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "to-lport",
		Priority:  ACLPriorityAllowDHCP,
		Match:     fmt.Sprintf("outport == @%s && udp && udp.src == 67 && udp.dst == 68", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-dhcp-ingress",
	}
}
