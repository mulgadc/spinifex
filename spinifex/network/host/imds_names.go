package host

// IMDS per-VPC veth naming. The names live here (not in network/topology) so
// the IMDS BindManager can derive them without importing topology, which would
// form an import cycle (topology imports handlers/imds for MetaDataServerIP).

// IMDSShortVPCID is the last 8 chars of the VPC ID. Linux IFNAMSIZ caps interface
// names at 15 chars, so the IMDS veth names key off this short form. Deterministic
// and stable: every chassis serving IMDS for VPC X derives the same names.
func IMDSShortVPCID(vpcID string) string {
	if len(vpcID) <= 8 {
		return vpcID
	}
	return vpcID[len(vpcID)-8:]
}

// IMDSOVSPortName is the br-int side of the per-VPC IMDS veth pair, bound to the
// imds-port LSP via external_ids:iface-id. The 7-char prefix keeps "imds-o-" + the
// 8-char short VPC ID at exactly IFNAMSIZ-1 (15); longer fails `ip link add`.
func IMDSOVSPortName(vpcID string) string { return "imds-o-" + IMDSShortVPCID(vpcID) }

// IMDSHostVethName is the host-end side of the per-VPC IMDS veth pair — the
// device the IMDS listener binds to via SO_BINDTODEVICE. Same 7-char prefix
// budget as IMDSOVSPortName: "imds-h-" + 8-char short VPC ID = 15 chars. It
// lives inside the VPC's netns (see IMDSNetnsName), not the root netns.
func IMDSHostVethName(vpcID string) string { return "imds-h-" + IMDSShortVPCID(vpcID) }

// IMDSNetnsName is the per-VPC network namespace the IMDS host-end veth and its
// listener live in. The netns gives the reply path a real L3 next-hop — the
// host end carries 169.254.169.254/30 with a default route via the .253 LRP —
// and structurally isolates VPCs with overlapping CIDRs, since each netns is its
// own routing domain. Netns names are filesystem-scoped, not IFNAMSIZ-bound, so
// "imds-" + 8-char short VPC ID (13 chars) is comfortably within budget.
func IMDSNetnsName(vpcID string) string { return "imds-" + IMDSShortVPCID(vpcID) }
