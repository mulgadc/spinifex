package policy

import (
	"fmt"
	"strconv"
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

	// ACLPriorityPlatformEgressBlock: platform WAN egress block (AWS-parity
	// outbound SMTP). Above tenant allows so a tenant SG cannot open it, below
	// DHCP so lease traffic is untouched. Not tenant-writable.
	ACLPriorityPlatformEgressBlock = 1049

	// ACLPriorityTenantAllow: tenant ingress/egress allows. Not logged.
	ACLPriorityTenantAllow = 1000

	// ACLPriorityAllowARP sits above the default-denies, which match every
	// ethertype. ACL tables run before the L2 lookup, so without this the
	// denies black-hole ARP and take IPv4 with it.
	ACLPriorityAllowARP = 950

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

// InfrastructureACLs returns the platform ACLs every PG carries: logged
// 900/800 default-denies (CMMC SC.L1-3.13.1), 1050 DHCPv4 allows and the 950
// ARP allows the denies would otherwise swallow.
func InfrastructureACLs(portGroupName string) []ovn.ACLSpec {
	return []ovn.ACLSpec{
		denyIngressACL(portGroupName),
		denyEgressACL(portGroupName),
		dhcpEgressACL(portGroupName),
		dhcpIngressACL(portGroupName),
		arpEgressACL(portGroupName),
		arpIngressACL(portGroupName),
	}
}

// wanEgressBlockPrivateDsts are the destinations the WAN egress block exempts,
// so intra-VPC and on-prem traffic (e.g. a private mail relay) is never dropped
// — only public destinations are blocked.
var wanEgressBlockPrivateDsts = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
}

// BlockedWANEgressACL drops guest egress to the given TCP destination ports for
// public destinations only — matching AWS, which blocks outbound SMTP out of the
// box so a compromised guest cannot become a spam relay. Logged for abuse
// triage. Returns ok=false when ports is empty, so a caller can skip it.
func BlockedWANEgressACL(portGroupName string, ports []int) (ovn.ACLSpec, bool) {
	if len(ports) == 0 {
		return ovn.ACLSpec{}, false
	}
	portStrs := make([]string, len(ports))
	for i, p := range ports {
		portStrs[i] = strconv.Itoa(p)
	}
	match := fmt.Sprintf(
		"inport == @%s && ip4 && ip4.dst != {%s} && tcp && tcp.dst == {%s}",
		portGroupName,
		strings.Join(wanEgressBlockPrivateDsts, ", "),
		strings.Join(portStrs, ", "),
	)
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityPlatformEgressBlock,
		Match:     match,
		Action:    "drop",
		Name:      portGroupName + "-block-wan-egress",
		Log:       true,
		Severity:  denyACLSeverity,
	}, true
}

// RuleACLSpecs builds priority-1000 allow ACLs with "allow-related" action.
func RuleACLSpecs(portGroupName string, ingress, egress []Rule) ([]ovn.ACLSpec, error) {
	specs := make([]ovn.ACLSpec, 0, len(ingress)+len(egress))
	for _, rule := range ingress {
		match, err := BuildIngressACLMatch(portGroupName, rule)
		if err != nil {
			return nil, err
		}
		specs = append(specs, ovn.ACLSpec{
			Direction: "to-lport",
			Priority:  ACLPriorityTenantAllow,
			Match:     match,
			Action:    "allow-related",
		})
	}
	for _, rule := range egress {
		match, err := BuildEgressACLMatch(portGroupName, rule)
		if err != nil {
			return nil, err
		}
		specs = append(specs, ovn.ACLSpec{
			Direction: "from-lport",
			Priority:  ACLPriorityTenantAllow,
			Match:     match,
			Action:    "allow-related",
		})
	}
	return specs, nil
}

// BuildIngressACLMatch builds an OVN to-lport match expression.
func BuildIngressACLMatch(portGroupName string, rule Rule) (string, error) {
	parts := []string{fmt.Sprintf("outport == @%s", portGroupName), "ip4"}
	parts, err := appendProtocolMatch(parts, rule)
	if err != nil {
		return "", err
	}
	if rule.CIDR != "" && rule.CIDR != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.src == %s", rule.CIDR))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.src == $%s", addressSetName(topology.SecurityGroupPortGroup(rule.SourceSG))))
	}
	return strings.Join(parts, " && "), nil
}

// BuildEgressACLMatch builds an OVN from-lport match.
func BuildEgressACLMatch(portGroupName string, rule Rule) (string, error) {
	parts := []string{fmt.Sprintf("inport == @%s", portGroupName), "ip4"}
	parts, err := appendProtocolMatch(parts, rule)
	if err != nil {
		return "", err
	}
	if rule.CIDR != "" && rule.CIDR != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.dst == %s", rule.CIDR))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.dst == $%s", addressSetName(topology.SecurityGroupPortGroup(rule.SourceSG))))
	}
	return strings.Join(parts, " && "), nil
}

// addressSetName returns the ovn-northd-derived SB Address_Set name for a PG's
// IPv4 addresses. Do NOT create NB Address_Set rows with this name — it wedges northd.
func addressSetName(portGroupName string) string {
	return portGroupName + "_ip4"
}

// appendProtocolMatch errors on an unrecognised protocol rather than emitting
// no L4 predicate: a silent no-op here widens the rule to every IP protocol.
// Callers must normalise numeric IANA values (6, 17, 1) to names first.
func appendProtocolMatch(parts []string, rule Rule) ([]string, error) {
	switch rule.IPProtocol {
	case "tcp":
		return appendPortMatch(parts, "tcp", rule.FromPort, rule.ToPort), nil
	case "udp":
		return appendPortMatch(parts, "udp", rule.FromPort, rule.ToPort), nil
	case "icmp":
		return append(parts, "icmp4"), nil
	case "-1", "":
		return parts, nil
	default:
		return nil, fmt.Errorf("unsupported IP protocol %q", rule.IPProtocol)
	}
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

// denyIngressACL carries no ethertype qualifier: OVN allows on no-match, so an
// ip4-scoped deny lets every other ethertype through. ARP is exempted at 950.
func denyIngressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "to-lport",
		Priority:  ACLPriorityDefaultDenyIngress,
		Match:     fmt.Sprintf("outport == @%s", portGroupName),
		Action:    "drop",
		Name:      portGroupName + "-deny-ingress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

// denyEgressACL is unqualified for the same reason as denyIngressACL.
func denyEgressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityDefaultDenyEgress,
		Match:     fmt.Sprintf("inport == @%s", portGroupName),
		Action:    "drop",
		Name:      portGroupName + "-deny-egress",
		Log:       true,
		Severity:  denyACLSeverity,
	}
}

// arpEgressACL: ARP out. No IPv6 counterpart — nothing in the stack assigns a
// guest a routable IPv6 address, so ND and DHCPv6 stay denied rather than
// becoming an unpoliced side channel.
func arpEgressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "from-lport",
		Priority:  ACLPriorityAllowARP,
		Match:     fmt.Sprintf("inport == @%s && arp", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-arp-egress",
	}
}

// arpIngressACL: ARP in. "allow" not "allow-related" — ARP is not an IP
// protocol and has no conntrack state to relate to.
func arpIngressACL(portGroupName string) ovn.ACLSpec {
	return ovn.ACLSpec{
		Direction: "to-lport",
		Priority:  ACLPriorityAllowARP,
		Match:     fmt.Sprintf("outport == @%s && arp", portGroupName),
		Action:    "allow",
		Name:      portGroupName + "-allow-arp-ingress",
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
