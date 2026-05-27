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

	// NATModeDistributed sets ExternalMAC + LogicalPort on every
	// dnat_and_snat so OVN processes DNAT on the VM's own chassis. Required
	// by UplinkModePhysical.
	NATModeDistributed

	// NATModeCentralized leaves ExternalMAC/LogicalPort unset so the gateway
	// chassis owns SNAT/DNAT. Required by UplinkModeVeth (bridge breaks
	// distributed-NAT hairpin routing).
	NATModeCentralized
)

func (m NATMode) String() string {
	switch m {
	case NATModeDistributed:
		return "distributed"
	case NATModeCentralized:
		return "centralized"
	default:
		return "unknown"
	}
}

// NATModeFromUplinkMode maps L0 uplink mode to NAT mode. Unknown maps to
// NATModeUnknown so misconfiguration fails loudly at construction.
func NATModeFromUplinkMode(m host.UplinkMode) NATMode {
	switch m {
	case host.UplinkModePhysical:
		return NATModeDistributed
	case host.UplinkModeVeth:
		return NATModeCentralized
	default:
		return NATModeUnknown
	}
}
