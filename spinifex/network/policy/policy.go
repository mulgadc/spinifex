// Package policy (L3) attaches data-plane policy — ACLs, NAT, routes — to
// L2 logical objects. It never creates or deletes L2 objects; missing
// targets surface as L1 "not found" errors.
package policy

import (
	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// NATMode selects distributed vs. centralised NAT (ADR-0006 S3). Fixed at
// NATManager construction from the L0 uplink mode.
type NATMode int

const (
	NATModeUnknown NATMode = iota

	// NATModeDistributed sets ExternalMAC+LogicalPort so OVN processes DNAT on
	// the VM's own chassis. The gateway LRP stays link-local, so a VPC consumes
	// no address from the external pool. Used by physical and veth uplinks.
	NATModeDistributed

	// NATModeCentralized leaves ExternalMAC/LogicalPort unset; gateway chassis
	// owns SNAT/DNAT and its LRP takes a real pool address.
	NATModeCentralized

	// NATModeRouted is centralised-shaped: gateway chassis SNATs the VPC CIDR
	// to its transit LRP IP; the host masquerades egress. Required by
	// UplinkModeRouted. Outbound-only — no EIP/public-IP support.
	NATModeRouted
)

func (m NATMode) String() string {
	switch m {
	case NATModeDistributed:
		return "distributed"
	case NATModeCentralized:
		return "centralized"
	case NATModeRouted:
		return "routed"
	default:
		return "unknown"
	}
}

// NATModeFromUplinkMode maps L0 uplink mode to NAT mode. Unknown maps to
// NATModeUnknown so misconfiguration fails loudly at construction.
func NATModeFromUplinkMode(m host.UplinkMode) NATMode {
	switch m {
	case host.UplinkModePhysical, host.UplinkModeVeth:
		// A veth uplink carries distributed NAT fine: the Linux bridge learns the
		// per-rule external MAC on the veth port like any other MAC. Only the
		// host's own uplink address is bridge-owned, and NAT never touches it.
		return NATModeDistributed
	case host.UplinkModeRouted:
		return NATModeRouted
	default:
		return NATModeUnknown
	}
}
