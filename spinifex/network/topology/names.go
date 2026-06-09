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

// ExternalSwitch is the OVN logical switch name for the per-VPC external
// switch bridging the gateway LRP to the localnet.
func ExternalSwitch(vpcID string) string { return "ext-" + vpcID }

// ExternalLocalnetPort is the OVN LSP name for the localnet bridging the
// external switch onto the host uplink.
func ExternalLocalnetPort(vpcID string) string { return "ext-port-" + vpcID }

// IMDSPort is the host-owned localport LSP that claims 169.254.169.254 directly
// on the subnet switch (SubnetSwitch). ovn-controller binds it on every chassis
// with a matching iface-id OVS port, so each chassis self-serves IMDS for its
// local VMs over a single L2 hop on the guest's own broadcast domain.
func IMDSPort(subnetID string) string { return "imds-port-" + subnetID }

// SecurityGroupPortGroup is the OVN port group name for an SG.
// OVN names match [a-zA-Z_][a-zA-Z0-9_]*, so hyphens become underscores.
func SecurityGroupPortGroup(sgID string) string {
	return strings.ReplaceAll(sgID, "-", "_")
}

// TransitSwitch is the OVN-IC transit switch name used for federation.
func TransitSwitch(azID, vpcID string) string { return "ts-" + azID + "-" + vpcID }

// TransitRouterPort is the OVN-IC transit router port name.
func TransitRouterPort(azID, vpcID string) string { return "trp-" + azID + "-" + vpcID }
