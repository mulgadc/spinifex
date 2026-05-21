package policy

import (
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// ACL priority table. Higher priority wins. Tenants only ever populate
// priority 1000; the 1100/1050/900/800 bands are platform-reserved.
//
// See docs/development/feature/spinifex-network-redesign.md §8.1 for the
// authoritative table.
const (
	// ACLPriorityAllowIntraSG allows unconditional traffic between members
	// of the same port group. Reserved for future use; not yet emitted by
	// InfrastructureACLs.
	ACLPriorityAllowIntraSG = 1100

	// ACLPriorityAllowDHCP sits above tenant rules so a narrow-egress SG
	// (e.g. only-VPC-CIDR) doesn't silently drop the guest's DHCPDISCOVER
	// against the priority-900 default-deny. AWS users can't write a rule
	// for 255.255.255.255, so the platform must.
	ACLPriorityAllowDHCP = 1050

	// ACLPriorityTenantAllow is the band tenant ingress/egress allow rules
	// emit at. Allows are not logged — high volume, low signal on a private
	// network.
	ACLPriorityTenantAllow = 1000

	// ACLPriorityDefaultDenyIngress is the logged drop for unmatched
	// ingress. Severity = denyACLSeverity. CMMC SC.L1-3.13.1.
	ACLPriorityDefaultDenyIngress = 900

	// ACLPriorityDefaultDenyEgress mirrors the ingress default-deny on the
	// egress direction.
	ACLPriorityDefaultDenyEgress = 800
)

// denyACLSeverity is the syslog severity OVN logs against a default-deny ACL
// hit. "info" is loud enough for a syslog forwarder to capture, quiet enough
// that a port scan doesn't page anyone. Operators can promote it at the log
// collector if they want higher-priority alerts.
const denyACLSeverity = "info"

// Rule is the policy-layer representation of an AWS-style security group
// rule. The handler layer constructs Rules from the AWS API payload after
// validating CidrIp / SourceSG — this builder interpolates the values
// verbatim, so untrusted input must not reach here.
type Rule struct {
	IPProtocol string // "tcp", "udp", "icmp", or "-1" (all)
	FromPort   int64
	ToPort     int64
	CIDR       string
	SourceSG   string
}

// InfrastructureACLs returns the platform ACLs that every port group carries
// regardless of tenant rules: the priority-900/800 default-deny pair (logged
// for CMMC SC.L1-3.13.1 boundary monitoring) and the priority-1050 DHCPv4
// client/server allows.
//
// OVN's built-in DHCP responder runs inside the LS pipeline after the
// port-group ACL stage, so without these allows a narrow-egress tenant SG
// silently drops the guest's DHCPDISCOVER (dst=255.255.255.255) against the
// default-deny and the VM never gets a lease. Matches AWS's implicit allow.
func InfrastructureACLs(portGroupName string) []ovn.ACLSpec {
	return []ovn.ACLSpec{
		denyIngressACL(portGroupName),
		denyEgressACL(portGroupName),
		dhcpEgressACL(portGroupName),
		dhcpIngressACL(portGroupName),
	}
}

// RuleACLSpecs builds the priority-1000 allow ACL specs for the given
// tenant rules. Action is "allow-related" so reply traffic for established
// flows passes without requiring an explicit reverse-direction rule.
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

// BuildIngressACLMatch generates an OVN ACL match expression for an ingress
// rule. Ingress rules use "outport == @{pgName}" because OVN ACLs with
// direction "to-lport" match on the destination port (outport from the
// pipeline's perspective).
//
// Tenant-supplied CIDR/SourceSG values MUST be validated by the handler
// (see handlers/ec2/vpc/security_group.go) before reaching this builder;
// values are interpolated verbatim.
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

// BuildEgressACLMatch generates an OVN ACL match expression for an egress
// rule. Egress rules use "inport == @{pgName}" because OVN ACLs with
// direction "from-lport" match on the source port (inport from the
// pipeline's perspective).
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

// addressSetName returns the SB Address_Set name that ovn-northd
// auto-derives for the given port group's IPv4 addresses. SG-to-SG ACL
// matches reference it via "ip4.src == $<name>" / "ip4.dst == $<name>".
// Lifecycle is owned by ovn-northd — Spinifex must NOT create explicit NB
// Address_Set rows with these names; a duplicate sync to SB wedges northd
// in an "OVNSB commit failed" retry loop.
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
		// All protocols — no additional filter beyond "ip4".
	default:
		// Unknown protocol — treat as all traffic.
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

// dhcpEgressACL allows the guest's DHCP client traffic (DHCPDISCOVER,
// DHCPREQUEST: udp.src=68, udp.dst=67) out of the port. Action is "allow"
// rather than "allow-related" — DHCP is single-shot request/reply and
// allow-related on broadcast UDP interacts oddly with OVN's CT zones.
func dhcpEgressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityAllowDHCP,
		Match:     fmt.Sprintf("inport == @%s && udp && udp.src == 68 && udp.dst == 67", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-dhcp-egress",
	}
}

// dhcpIngressACL allows the OVN-generated DHCP server reply (DHCPOFFER,
// DHCPACK: udp.src=67, udp.dst=68) back to the guest. OVN's DHCP responder
// emits the reply from inside the LS pipeline, so without this matching
// to-lport allow the reply hits the priority-900 ingress default-deny.
func dhcpIngressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "to-lport",
		Priority:  ACLPriorityAllowDHCP,
		Match:     fmt.Sprintf("outport == @%s && udp && udp.src == 67 && udp.dst == 68", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-dhcp-ingress",
	}
}
