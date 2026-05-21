package vpcd

// Phase 1 adapter: TopologyHandler implements topology.Manager by wrapping
// the same OVN operations the NATS handlers (handleVPCCreate / handleSubnetCreate
// / handleCreatePort / etc.) drive. The Manager API is callable on the existing
// in-tree handler; Phase 2 replaces the NATS handlers with thin shims that
// route to these methods, and Phase 3 deletes the handlers entirely.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
)

var _ topology.Manager = (*TopologyHandler)(nil)

// EnsureVPC creates the OVN logical router for the VPC, idempotently. Mirrors
// handleVPCCreate's OVN ops without the NATS envelope.
func (h *TopologyHandler) EnsureVPC(ctx context.Context, spec topology.VPCSpec) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.VPCID == "" {
		return fmt.Errorf("EnsureVPC: empty VPCID")
	}
	routerName := topology.VPCRouter(spec.VPCID)
	if _, err := h.ovn.GetLogicalRouter(ctx, routerName); err == nil {
		return nil
	}
	cidr := ""
	if spec.CIDR.IsValid() {
		cidr = spec.CIDR.String()
	}
	lr := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:vni":    strconv.FormatInt(spec.VNI, 10),
			"spinifex:cidr":   cidr,
		},
	}
	if err := h.ovn.CreateLogicalRouter(ctx, lr); err != nil {
		return fmt.Errorf("create logical router %q: %w", routerName, err)
	}
	slog.Info("vpcd: EnsureVPC created router", "router", routerName, "vpc_id", spec.VPCID, "cidr", cidr)
	return nil
}

// DeleteVPC removes the OVN logical router for the VPC and cascades through
// any subnets that belong to it. Mirrors handleVPCDelete's OVN ops.
func (h *TopologyHandler) DeleteVPC(ctx context.Context, vpcID string) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	routerName := topology.VPCRouter(vpcID)

	switches, err := h.ovn.ListLogicalSwitches(ctx)
	if err != nil {
		slog.Warn("vpcd: DeleteVPC list switches", "err", err)
	} else {
		for _, ls := range switches {
			if ls.ExternalIDs["spinifex:vpc_id"] != vpcID {
				continue
			}
			if err := h.ovn.DeleteLogicalSwitch(ctx, ls.Name); err != nil {
				slog.Warn("vpcd: DeleteVPC cascade switch", "switch", ls.Name, "err", err)
			}
		}
	}

	dhcpOpts, err := h.ovn.ListDHCPOptions(ctx)
	if err != nil {
		slog.Warn("vpcd: DeleteVPC list DHCP options", "err", err)
	} else {
		for _, opts := range dhcpOpts {
			if opts.ExternalIDs["spinifex:vpc_id"] != vpcID {
				continue
			}
			if err := h.ovn.DeleteDHCPOptions(ctx, opts.UUID); err != nil {
				slog.Warn("vpcd: DeleteVPC cascade DHCP", "uuid", opts.UUID, "err", err)
			}
		}
	}

	if err := h.ovn.DeleteLogicalRouter(ctx, routerName); err != nil {
		return fmt.Errorf("delete logical router %q: %w", routerName, err)
	}
	slog.Info("vpcd: DeleteVPC removed router", "router", routerName, "vpc_id", vpcID)
	return nil
}

// EnsureSubnet creates the OVN logical switch, subnet→VPC router port pair,
// and DHCP options for the subnet. Mirrors handleSubnetCreate's OVN ops.
func (h *TopologyHandler) EnsureSubnet(ctx context.Context, spec topology.SubnetSpec) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.SubnetID == "" || spec.VPCID == "" {
		return fmt.Errorf("EnsureSubnet: SubnetID/VPCID required")
	}
	if !spec.CIDR.IsValid() {
		return fmt.Errorf("EnsureSubnet: invalid CIDR for subnet %q", spec.SubnetID)
	}
	cidr := spec.CIDR.String()
	switchName := topology.SubnetSwitch(spec.SubnetID)
	routerName := topology.VPCRouter(spec.VPCID)
	routerPortName := topology.SubnetRouterPort(spec.SubnetID)
	switchRouterPortName := topology.SubnetSwitchRouterPort(spec.SubnetID)

	gwIP, mask, err := subnetGateway(cidr)
	if err != nil {
		return fmt.Errorf("invalid subnet CIDR %q: %w", cidr, err)
	}
	gwCIDR := fmt.Sprintf("%s/%d", gwIP, mask)
	routerMAC := generateMAC(spec.SubnetID)

	if _, err := h.ovn.GetLogicalSwitch(ctx, switchName); err == nil {
		return nil
	}

	ls := &nbdb.LogicalSwitch{
		Name: switchName,
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := h.ovn.CreateLogicalSwitch(ctx, ls); err != nil {
		return fmt.Errorf("create logical switch %q: %w", switchName, err)
	}

	lrp := &nbdb.LogicalRouterPort{
		Name:     routerPortName,
		MAC:      routerMAC,
		Networks: []string{gwCIDR},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create router port %q: %w", routerPortName, err)
	}

	lsp := &nbdb.LogicalSwitchPort{
		Name:      switchRouterPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": routerPortName,
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
		_ = h.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName)
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create switch router port %q: %w", switchRouterPortName, err)
	}

	dhcpOpts := &nbdb.DHCPOptions{
		CIDR: cidr,
		Options: map[string]string{
			"server_id":  gwIP,
			"server_mac": routerMAC,
			"lease_time": "3600",
			"router":     gwIP,
			"dns_server": h.dnsServer(),
			"mtu":        "1442",
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if _, err := h.ovn.CreateDHCPOptions(ctx, dhcpOpts); err != nil {
		slog.Warn("vpcd: EnsureSubnet DHCP options create failed", "cidr", cidr, "err", err)
	}

	slog.Info("vpcd: EnsureSubnet created topology",
		"switch", switchName,
		"router_port", routerPortName,
		"gateway", gwCIDR,
		"subnet_id", spec.SubnetID,
	)
	return nil
}

// DeleteSubnet tears down the subnet's logical switch, router port, switch
// router port, and DHCP options. Mirrors handleSubnetDelete's OVN ops.
func (h *TopologyHandler) DeleteSubnet(ctx context.Context, spec topology.SubnetSpec) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	switchName := topology.SubnetSwitch(spec.SubnetID)
	routerName := topology.VPCRouter(spec.VPCID)
	routerPortName := topology.SubnetRouterPort(spec.SubnetID)
	switchRouterPortName := topology.SubnetSwitchRouterPort(spec.SubnetID)

	if err := h.ovn.DeleteLogicalSwitchPort(ctx, switchName, switchRouterPortName); err != nil {
		slog.Warn("vpcd: DeleteSubnet switch router port", "port", switchRouterPortName, "err", err)
	}
	if err := h.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName); err != nil {
		slog.Warn("vpcd: DeleteSubnet router port", "port", routerPortName, "err", err)
	}
	if spec.CIDR.IsValid() {
		if dhcpOpts, err := h.ovn.FindDHCPOptionsByCIDR(ctx, spec.CIDR.String()); err == nil {
			if err := h.ovn.DeleteDHCPOptions(ctx, dhcpOpts.UUID); err != nil {
				slog.Warn("vpcd: DeleteSubnet DHCP options", "cidr", spec.CIDR.String(), "err", err)
			}
		}
	}
	if err := h.ovn.DeleteLogicalSwitch(ctx, switchName); err != nil {
		return fmt.Errorf("delete logical switch %q: %w", switchName, err)
	}
	slog.Info("vpcd: DeleteSubnet removed topology", "switch", switchName, "subnet_id", spec.SubnetID)
	return nil
}

// EnsurePort creates the ENI's LogicalSwitchPort and joins it to its initial
// SG port groups in one OVSDB transaction. Mirrors handleCreatePort's OVN ops.
//
// Idempotent: if the LSP already exists from a crashed prior attempt that
// didn't reach the port-group join, SG memberships converge here rather than
// waiting for the next reconciler pass — that gap would leave a port with zero
// ACLs (OVN default = unrestricted).
func (h *TopologyHandler) EnsurePort(ctx context.Context, spec topology.PortSpec) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.PortID == "" || spec.SubnetID == "" {
		return fmt.Errorf("EnsurePort: PortID/SubnetID required")
	}
	portName := topology.Port(spec.PortID)
	switchName := topology.SubnetSwitch(spec.SubnetID)

	if _, err := h.ovn.GetLogicalSwitchPort(ctx, portName); err == nil {
		if _, err := h.reconcilePortSGs(ctx, portName, spec.SGIDs); err != nil {
			return fmt.Errorf("reconcile SGs for existing port %q: %w", portName, err)
		}
		return nil
	}

	addrStr := fmt.Sprintf("%s %s", spec.MAC.String(), spec.PrivateIP.String())
	lsp := &nbdb.LogicalSwitchPort{
		Name:         portName,
		Addresses:    []string{addrStr},
		PortSecurity: []string{addrStr},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    spec.PortID,
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if dhcpOpts, err := h.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", spec.SubnetID); err == nil {
		lsp.DHCPv4Options = &dhcpOpts.UUID
	}

	pgNames := make([]string, 0, len(spec.SGIDs))
	for _, sgID := range spec.SGIDs {
		pgNames = append(pgNames, topology.SecurityGroupPortGroup(sgID))
	}
	if err := h.ovn.CreateLogicalSwitchPortInGroups(ctx, switchName, lsp, pgNames); err != nil {
		return fmt.Errorf("create logical switch port %q on %q: %w", portName, switchName, err)
	}
	slog.Info("vpcd: EnsurePort created LSP",
		"port", portName,
		"switch", switchName,
		"eni_id", spec.PortID,
		"ip", spec.PrivateIP.String(),
		"mac", spec.MAC.String(),
		"addr_str", addrStr,
		"sgs", spec.SGIDs,
		"port_groups", pgNames,
	)
	return nil
}

// DeletePort clears the ENI's port-group memberships and removes the LSP.
// Mirrors handleDeletePort's OVN ops.
func (h *TopologyHandler) DeletePort(ctx context.Context, spec topology.PortSpec) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	portName := topology.Port(spec.PortID)
	switchName := topology.SubnetSwitch(spec.SubnetID)

	if _, err := h.reconcilePortSGs(ctx, portName, nil); err != nil {
		return fmt.Errorf("clear port group memberships for %q: %w", portName, err)
	}
	if err := h.ovn.DeleteLogicalSwitchPort(ctx, switchName, portName); err != nil {
		return fmt.Errorf("delete logical switch port %q on %q: %w", portName, switchName, err)
	}
	slog.Info("vpcd: DeletePort removed LSP",
		"port", portName,
		"switch", switchName,
		"eni_id", spec.PortID,
	)
	return nil
}

// SetPortSecurityGroups reconciles the port's port-group memberships against
// the declared list. Manager computes the add/remove diff via reconcilePortSGs.
func (h *TopologyHandler) SetPortSecurityGroups(ctx context.Context, portID string, sgIDs []string) error {
	if h.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	portName := topology.Port(portID)
	if _, err := h.reconcilePortSGs(ctx, portName, sgIDs); err != nil {
		return fmt.Errorf("reconcile SGs for port %q: %w", portName, err)
	}
	return nil
}
