package topology

import "strings"

// OVN object names form a stable contract; changes require a reconciliation migration.

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

// GatewayChassisRedirectPort is the OVN chassisredirect Port_Binding (cr-<LRP>).
// Only this binding carries the gateway-chassis claim; the LRP binding stays chassis-less.
func GatewayChassisRedirectPort(vpcID string) string { return "cr-" + GatewayRouterPort(vpcID) }

// ExternalSwitch is the OVN logical switch name for the legacy per-VPC external
// switch. Retained only so the reconciler/teardown can identify and remove
// pre-shared-switch deployments; new attaches use ExternalSwitchShared.
func ExternalSwitch(vpcID string) string { return "ext-" + vpcID }

// ExternalLocalnetPort is the legacy per-VPC localnet LSP name. Retained for
// legacy cleanup only; see ExternalLocalnetPortShared.
func ExternalLocalnetPort(vpcID string) string { return "ext-port-" + vpcID }

// ExternalSwitchShared is the single shared external logical switch. Every VPC
// gateway router port attaches here so only one untagged localnet exists per
// physical network — multiple per-VPC localnets on one uplink collide on L2.
func ExternalSwitchShared() string { return "ext-shared" }

// ExternalLocalnetPortShared is the single localnet LSP on the shared external
// switch bridging it onto the host uplink.
func ExternalLocalnetPortShared() string { return "ext-port-shared" }

// SecurityGroupPortGroup is the OVN port group name for an SG.
// OVN names match [a-zA-Z_][a-zA-Z0-9_]*, so hyphens become underscores.
func SecurityGroupPortGroup(sgID string) string {
	return strings.ReplaceAll(sgID, "-", "_")
}

// TransitSwitch is the OVN-IC transit switch name used for federation.
func TransitSwitch(azID, vpcID string) string { return "ts-" + azID + "-" + vpcID }

// TransitRouterPort is the OVN-IC transit router port name.
func TransitRouterPort(azID, vpcID string) string { return "trp-" + azID + "-" + vpcID }
