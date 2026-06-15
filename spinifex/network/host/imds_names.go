package host

// IMDS per-subnet veth naming lives here (not in network/topology) to avoid an
// import cycle: topology imports handlers/imds for MetaDataServerIP.

// IMDSShortSubnetID returns the last 8 chars of subnetID. Linux IFNAMSIZ caps
// interface names at 15 chars, so veth names key off this short form.
func IMDSShortSubnetID(subnetID string) string {
	if len(subnetID) <= 8 {
		return subnetID
	}
	return subnetID[len(subnetID)-8:]
}

// IMDSOVSPortName is the br-int side of the per-subnet IMDS veth pair, bound via
// external_ids:iface-id. "imds-o-" + 8-char short ID = 15 chars (IFNAMSIZ-1).
func IMDSOVSPortName(subnetID string) string { return "imds-o-" + IMDSShortSubnetID(subnetID) }

// IMDSHostVethName is the host-end of the IMDS veth pair (SO_BINDTODEVICE).
// "imds-h-" + 8-char short ID = 15 chars; lives in the subnet netns.
func IMDSHostVethName(subnetID string) string { return "imds-h-" + IMDSShortSubnetID(subnetID) }

// IMDSNetnsName is the per-subnet network namespace for the IMDS host-end veth.
// Isolates overlapping CIDRs into separate routing domains; not IFNAMSIZ-bound.
func IMDSNetnsName(subnetID string) string { return "imds-" + IMDSShortSubnetID(subnetID) }
