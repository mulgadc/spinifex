package vpcd

import (
	"fmt"
	"strings"
)

// SGRuleForACL mirrors the SGRule from handlers for vpcd's use.
type SGRuleForACL struct {
	IpProtocol string `json:"ip_protocol"` // "tcp", "udp", "icmp", "-1" (all)
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	SourceSG   string `json:"source_sg,omitempty"`
}

// portGroupName converts a security group ID to an OVN port group name.
// OVN port group names must match [a-zA-Z_][a-zA-Z0-9_]*, so hyphens
// in sg-xxx IDs are replaced with underscores.
func portGroupName(groupId string) string {
	return strings.ReplaceAll(groupId, "-", "_")
}

// addressSetName is the OVN address set holding the IPv4 addresses of every
// port that belongs to the given port group. SG-to-SG ACL match expressions
// reference it via "ip4.src == $<name>" / "ip4.dst == $<name>". Lifecycle is
// tied to the port group (created in handleCreateSG, deleted in handleDeleteSG);
// reconcilePortSGs maintains its membership in step with port-group joins.
func addressSetName(pgName string) string {
	return pgName + "_ip4"
}

// BuildIngressACLMatch generates an OVN ACL match expression for an ingress rule.
// Ingress rules use "outport == @{pgName}" because OVN ACLs with direction "to-lport"
// match on the destination port (outport from the pipeline's perspective).
//
// Tenant-supplied CidrIp/SourceSG values MUST be validated by the handler before
// reaching this builder (see ec2/vpc/security_group.go ipPermissionsToSGRules). This
// builder interpolates them verbatim.
func BuildIngressACLMatch(pgName string, rule SGRuleForACL) string {
	parts := []string{fmt.Sprintf("outport == @%s", pgName), "ip4"}

	// Protocol filter
	parts = appendProtocolMatch(parts, rule)

	// Source filter
	if rule.CidrIp != "" && rule.CidrIp != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.src == %s", rule.CidrIp))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.src == $%s", addressSetName(portGroupName(rule.SourceSG))))
	}

	return strings.Join(parts, " && ")
}

// BuildEgressACLMatch generates an OVN ACL match expression for an egress rule.
// Egress rules use "inport == @{pgName}" because OVN ACLs with direction "from-lport"
// match on the source port (inport from the pipeline's perspective).
func BuildEgressACLMatch(pgName string, rule SGRuleForACL) string {
	parts := []string{fmt.Sprintf("inport == @%s", pgName), "ip4"}

	// Protocol filter
	parts = appendProtocolMatch(parts, rule)

	// Destination filter
	if rule.CidrIp != "" && rule.CidrIp != "0.0.0.0/0" {
		parts = append(parts, fmt.Sprintf("ip4.dst == %s", rule.CidrIp))
	}
	if rule.SourceSG != "" {
		parts = append(parts, fmt.Sprintf("ip4.dst == $%s", addressSetName(portGroupName(rule.SourceSG))))
	}

	return strings.Join(parts, " && ")
}

// appendProtocolMatch adds protocol-specific match clauses to the parts slice.
func appendProtocolMatch(parts []string, rule SGRuleForACL) []string {
	switch rule.IpProtocol {
	case "tcp":
		parts = appendPortMatch(parts, "tcp", rule.FromPort, rule.ToPort)
	case "udp":
		parts = appendPortMatch(parts, "udp", rule.FromPort, rule.ToPort)
	case "icmp":
		parts = append(parts, "icmp4")
	case "-1", "":
		// All protocols — no additional filter needed beyond "ip4"
	default:
		// Unknown protocol — treat as all traffic
	}
	return parts
}

// appendPortMatch adds TCP/UDP port match clauses.
func appendPortMatch(parts []string, proto string, fromPort, toPort int64) []string {
	if fromPort == 0 && toPort == 0 {
		// All ports for this protocol
		parts = append(parts, proto)
		return parts
	}
	if fromPort == toPort {
		// Single port
		parts = append(parts, fmt.Sprintf("%s.dst == %d", proto, fromPort))
	} else {
		// Port range
		parts = append(parts, fmt.Sprintf("%s.dst >= %d", proto, fromPort))
		parts = append(parts, fmt.Sprintf("%s.dst <= %d", proto, toPort))
	}
	return parts
}
