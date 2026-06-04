package host

// IMDS per-subnet veth naming. The names live here (not in network/topology) so
// the IMDS BindManager can derive them without importing topology, which would
// form an import cycle (topology imports handlers/imds for MetaDataServerIP).

// IMDSShortSubnetID is the last 8 chars of the subnet ID. Linux IFNAMSIZ caps
// interface names at 15 chars, so the IMDS veth names key off this short form.
// Deterministic and stable: every chassis serving IMDS for subnet X derives the
// same names.
func IMDSShortSubnetID(subnetID string) string {
	if len(subnetID) <= 8 {
		return subnetID
	}
	return subnetID[len(subnetID)-8:]
}

// IMDSOVSPortName is the br-int side of the per-subnet IMDS veth pair, bound to the
// imds-port LSP via external_ids:iface-id. The 7-char prefix keeps "imds-o-" + the
// 8-char short subnet ID at exactly IFNAMSIZ-1 (15); longer fails `ip link add`.
func IMDSOVSPortName(subnetID string) string { return "imds-o-" + IMDSShortSubnetID(subnetID) }

// IMDSHostVethName is the host-end side of the per-subnet IMDS veth pair — the
// device the IMDS listener binds to via SO_BINDTODEVICE. Same 7-char prefix
// budget as IMDSOVSPortName: "imds-h-" + 8-char short subnet ID = 15 chars. It
// lives inside the subnet's netns (see IMDSNetnsName), not the root netns.
func IMDSHostVethName(subnetID string) string { return "imds-h-" + IMDSShortSubnetID(subnetID) }

// IMDSNetnsName is the per-subnet network namespace the IMDS host-end veth and its
// listener live in. The netns gives the reply path a real L3 next-hop — the host
// end carries 169.254.169.254/30 with the subnet CIDR on-link, so the reply to the
// guest resolves by ARP over the localport — and structurally isolates subnets
// with overlapping CIDRs, since each netns is its own routing domain. Netns names
// are filesystem-scoped, not IFNAMSIZ-bound, so "imds-" + 8-char short subnet ID
// (13 chars) is comfortably within budget.
func IMDSNetnsName(subnetID string) string { return "imds-" + IMDSShortSubnetID(subnetID) }
