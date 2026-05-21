// Package policy is L3 of the spinifex network stack: data-plane policy
// attached to the logical objects owned by L2 (network/topology). It exposes
// three managers — SecurityGroupManager (ACLs against SG port groups),
// NATManager (dnat_and_snat for EIPs, snat for NAT gateways and IGW default
// outbound), and RouteManager (static + default routes) — and calls L1
// (network/ovn) for the OVSDB transactions.
//
// L3 does not create or delete L2 objects. Port groups, logical routers, and
// logical switches must already exist before the L3 managers can attach
// policy to them; layering breaks are loud (L1 returns "not found" and the
// L3 method propagates the error).
//
// See docs/development/feature/spinifex-network-redesign.md §8 and
// docs/development/feature/spinifex-network-redesign-phase2.md §2.1 for the
// full contract.
package policy

import (
	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// NATMode tells NATManager whether the cluster runs distributed or
// centralised NAT. The choice is fixed at L0 by the host's uplink mode and
// is passed in at NATManager construction time — there is no runtime
// reselection. ADR-0006 S3.
type NATMode int

const (
	// NATModeUnknown is the zero value; never returned by a configured
	// NATManager.
	NATModeUnknown NATMode = iota

	// NATModeDistributed sets ExternalMAC + LogicalPort on every dnat_and_snat
	// rule so OVN processes DNAT on the VM's own chassis without hairpinning
	// through the gateway chassis. Required by UplinkModePhysical.
	NATModeDistributed

	// NATModeCentralized leaves ExternalMAC + LogicalPort unset so the
	// gateway chassis owns SNAT/DNAT. Required by UplinkModeVeth because the
	// Linux bridge intermediary breaks distributed-NAT hairpin routing.
	NATModeCentralized
)

// String returns the canonical name used in logs.
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

// NATModeFromUplinkMode resolves the L0 uplink mode into the L3 NAT mode.
// The mapping is fixed: physical uplinks support per-chassis NAT, veth
// uplinks do not. UplinkModeUnknown returns NATModeUnknown so misconfiguration
// surfaces loudly at NATManager construction.
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
