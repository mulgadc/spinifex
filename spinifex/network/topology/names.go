package topology

import "strings"

// OVN object names are deterministic and form a stable contract. Changing any
// of them requires a reconciliation migration. The §7.3 target state in
// docs/development/feature/spinifex-network-redesign.md lists `rp-{subnetID}`
// for the subnet→VPC router port and `ln-ext-{vpcID}` for the external
// localnet port; Phase 1 keeps the existing in-tree names (`rtr-{subnetID}` /
// `ext-port-{vpcID}`) to preserve compatibility with deployed clusters. The
// rename happens in a later phase alongside a reconciler-driven migration.

// VPCRouter is the OVN logical router name for a VPC.
func VPCRouter(vpcID string) string { return "vpc-" + vpcID }

// SubnetSwitch is the OVN logical switch name for a subnet.
func SubnetSwitch(subnetID string) string { return "subnet-" + subnetID }

// SubnetRouterPort is the OVN LRP name on the VPC router that terminates the
// subnet's default gateway. Existing name; §7.3 target `rp-{subnetID}`.
func SubnetRouterPort(subnetID string) string { return "rtr-" + subnetID }

// SubnetSwitchRouterPort is the OVN LSP (type=router) on the subnet switch
// peered with SubnetRouterPort.
func SubnetSwitchRouterPort(subnetID string) string { return "rtr-port-" + subnetID }

// Port is the OVN logical switch port name for an ENI.
func Port(portID string) string { return "port-" + portID }

// GatewayRouterPort is the OVN LRP name for the VPC's uplink/gateway port.
func GatewayRouterPort(vpcID string) string { return "gw-" + vpcID }

// ExternalSwitch is the OVN logical switch name for the per-VPC external
// switch bridging the gateway LRP to the localnet.
func ExternalSwitch(vpcID string) string { return "ext-" + vpcID }

// ExternalLocalnetPort is the OVN LSP name for the localnet bridging the
// external switch onto the host uplink. Existing name; §7.3 target
// `ln-ext-{vpcID}`.
func ExternalLocalnetPort(vpcID string) string { return "ext-port-" + vpcID }

// SecurityGroupPortGroup is the OVN port group name for a security group.
// OVN port group names match [a-zA-Z_][a-zA-Z0-9_]*, so hyphens in sg-xxx
// IDs are replaced with underscores. The per-port-group `_ip4`/`_ip6`
// Address_Set rows in SB are auto-derived by ovn-northd from each port
// group's port addresses. §7.3 of the network redesign plan lists
// `sg-{sgID}` with hyphens preserved; this conflicts with the OVN naming
// regex and existing in-tree behaviour, which uses underscores.
func SecurityGroupPortGroup(sgID string) string {
	return strings.ReplaceAll(sgID, "-", "_")
}

// TransitSwitch is the OVN-IC transit switch name (Phase 3+ federation).
func TransitSwitch(azID, vpcID string) string { return "ts-" + azID + "-" + vpcID }

// TransitRouterPort is the OVN-IC transit router port name (Phase 3+).
func TransitRouterPort(azID, vpcID string) string { return "trp-" + azID + "-" + vpcID }
