package policy

import (
	"net/netip"
	"slices"
)

// PrefixLists resolves a prefix-list ID to its entries' CIDRs; ok is false
// for a list it does not know.
type PrefixLists func(id string) (cidrs []string, ok bool)

// EgressAllows reports whether egress rules, the union of every group on an
// ENI, allow a new connection to a host-owned dst, such as an S3 endpoint
// address, which no security group lists as a member.
//
// It answers what BuildEgressACLMatch's ACLs would decide for that connection,
// for traffic that bypasses them. A rule naming a security group grants
// nothing, because dst is never in a group's address set. A rule naming a
// prefix list grants what its entries in lists would, and nothing for a list
// lists does not know. A rule with no destination, or with a protocol the
// compiler refuses, also grants nothing, where the compiler would match
// everything or fail.
func EgressAllows(rules []Rule, lists PrefixLists, dst netip.Addr, proto string, port uint16) bool {
	if !dst.Is4() {
		return false
	}
	for _, r := range rules {
		if r.SourceSG != "" || !ruleContains(r, lists, dst) {
			continue
		}
		if protocolAllows(r, proto, port) {
			return true
		}
	}
	return false
}

// ruleContains reports whether dst is in the rule's CIDR and prefix list, as
// the compiler's conjunction of the two terms would.
func ruleContains(r Rule, lists PrefixLists, dst netip.Addr) bool {
	if r.CIDR == "" && r.PrefixListID == "" {
		return false
	}
	if r.CIDR != "" && !cidrContains(r.CIDR, dst) {
		return false
	}
	if r.PrefixListID == "" {
		return true
	}
	if lists == nil || !prefixListIDRegex.MatchString(r.PrefixListID) {
		return false
	}
	cidrs, ok := lists(r.PrefixListID)
	return ok && slices.ContainsFunc(cidrs, func(c string) bool { return cidrContains(c, dst) })
}

// cidrContains reads CIDR as the compiler does: a prefix or a lone address,
// IPv4 only.
func cidrContains(cidr string, dst netip.Addr) bool {
	if p, err := netip.ParsePrefix(cidr); err == nil {
		return p.Addr().Is4() && p.Masked().Contains(dst)
	}
	a, err := netip.ParseAddr(cidr)
	return err == nil && a == dst
}

// protocolAllows mirrors appendProtocolMatch and appendPortMatch: 0-0 is every
// port, an equal pair is that port alone, and anything else is the range.
func protocolAllows(r Rule, proto string, port uint16) bool {
	switch r.IPProtocol {
	case "-1", "":
		return true
	case "tcp", "udp":
		if r.IPProtocol != proto {
			return false
		}
		p := int64(port)
		if r.FromPort == 0 && r.ToPort == 0 {
			return true
		}
		if r.FromPort == r.ToPort {
			return p == r.FromPort
		}
		return p >= r.FromPort && p <= r.ToPort
	default:
		// icmp carries no port and never matches tcp or udp; anything else
		// is refused by the compiler.
		return false
	}
}
