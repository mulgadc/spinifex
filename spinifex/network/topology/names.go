package topology

import "strings"

// OVN object names are deterministic and form a stable contract. Changing any
// of them requires a reconciliation migration.

// VPCRouter is the OVN logical router name for a VPC.
func VPCRouter(vpcID string) string { return "vpc-" + vpcID }

// SubnetSwitch is the OVN logical switch name for a subnet.
func SubnetSwitch(subnetID string) string { return "subnet-" + subnetID }

// SubnetRouterPort is the OVN LRP name on the VPC router that terminates the
// subnet's default gateway.
func SubnetRouterPort(subnetID string) string { return "rtr-" + subnetID }

// SubnetSwitchRouterPort is the OVN LSP (type=router) on the subnet switch
// peered with SubnetRouterPort.
func SubnetSwitchRouterPort(subnetID string) string { return "rtr-port-" + subnetID }

// Port is the OVN logical switch port name for an ENI.
func Port(portID string) string { return "port-" + portID }

// GatewayRouterPort is the OVN LRP name for the VPC's uplink/gateway port.
func GatewayRouterPort(vpcID string) string { return "gw-" + vpcID }

// GatewaySwitchPort is the OVN LSP (type=router) on the external switch
// peered with GatewayRouterPort.
func GatewaySwitchPort(vpcID string) string { return "gw-port-" + vpcID }

// ExternalSwitch is the OVN logical switch name for the per-VPC external
// switch bridging the gateway LRP to the localnet.
func ExternalSwitch(vpcID string) string { return "ext-" + vpcID }

// ExternalLocalnetPort is the OVN LSP name for the localnet bridging the
// external switch onto the host uplink.
func ExternalLocalnetPort(vpcID string) string { return "ext-port-" + vpcID }

// SecurityGroupPortGroup is the OVN port group name for a security group.
// OVN port group names match [a-zA-Z_][a-zA-Z0-9_]*, so hyphens in sg-xxx
// IDs are replaced with underscores. The per-port-group `_ip4`/`_ip6`
// Address_Set rows in SB are auto-derived by ovn-northd from each port
// group's port addresses.
func SecurityGroupPortGroup(sgID string) string {
	return strings.ReplaceAll(sgID, "-", "_")
}

// TransitSwitch is the OVN-IC transit switch name used for federation.
func TransitSwitch(azID, vpcID string) string { return "ts-" + azID + "-" + vpcID }

// TransitRouterPort is the OVN-IC transit router port name.
func TransitRouterPort(azID, vpcID string) string { return "trp-" + azID + "-" + vpcID }
