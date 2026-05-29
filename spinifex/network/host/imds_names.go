package host

// IMDS per-VPC veth naming. The names live here (not in network/topology) so
// the IMDS BindManager can derive them without importing topology, which would
// form an import cycle (topology imports handlers/imds for MetaDataServerIP).

// IMDSShortVPCID is the last 8 chars of the VPC ID. Linux IFNAMSIZ caps
// interface names at 15 chars, so the IMDS veth names key off this short form
// rather than the full VPC ID. Deterministic and stable across reboots and
// chassis: every chassis serving IMDS for VPC X derives the same names.
func IMDSShortVPCID(vpcID string) string {
	if len(vpcID) <= 8 {
		return vpcID
	}
	return vpcID[len(vpcID)-8:]
}

// IMDSOVSPortName is the br-int side of the per-VPC IMDS veth pair, bound to
// the imds-port LSP via external_ids:iface-id. The prefix is held to 7 chars so
// "imds-o-" + the 8-char short VPC ID is exactly IFNAMSIZ-1 (15); any longer and
// `ip link add` fails with "name too long" for every VPC.
func IMDSOVSPortName(vpcID string) string { return "imds-o-" + IMDSShortVPCID(vpcID) }

// IMDSHostVethName is the root-netns side of the per-VPC IMDS veth pair — the
// device the IMDS listener binds to via SO_BINDTODEVICE. Same 7-char prefix
// budget as IMDSOVSPortName: "imds-h-" + 8-char short VPC ID = 15 chars.
func IMDSHostVethName(vpcID string) string { return "imds-h-" + IMDSShortVPCID(vpcID) }
