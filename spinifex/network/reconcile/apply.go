package reconcile

import (
	"context"
	"fmt"
	"log/slog"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// applyVPCs ensures every intent VPC has a LogicalRouter. Stray OVN-only
// routers are left alone.
func (r *reconciler) applyVPCs(ctx context.Context, intent IntentState, actual ActualState) {
	for vpcID, spec := range intent.VPCs {
		routerName := topology.VPCRouter(vpcID)
		if _, ok := actual.Routers[routerName]; !ok {
			lr := &nbdb.LogicalRouter{
				Name: routerName,
				ExternalIDs: map[string]string{
					"spinifex:vpc_id": vpcID,
					"spinifex:cidr":   spec.CIDR.String(),
				},
			}
			if _, err := r.ovn.EnsureLogicalRouter(ctx, lr); err != nil {
				slog.Error("reconcile/apply: ensure VPC router failed", "vpc_id", vpcID, "err", err)
				continue
			}
			actual.Routers[routerName] = struct{}{}
			slog.Info("reconcile/apply: ensured VPC router", "vpc_id", vpcID, "router", routerName)
		}
		// Install IMDS topology for every intent VPC (idempotent via the
		// vpc-veth bucket gate); the router must exist for the IMDS LRP.
		if r.imds != nil {
			if _, err := r.imds.EnsureForVPC(ctx, vpcID); err != nil {
				slog.Error("reconcile/apply: IMDS EnsureForVPC failed", "vpc_id", vpcID, "err", err)
			}
		}
	}
}

// applySubnets ensures every intent Subnet has a LogicalSwitch, parent-LRP,
// router-side LSP, and DHCPOptions row. Each step is independently idempotent.
func (r *reconciler) applySubnets(ctx context.Context, intent IntentState, actual ActualState) {
	for subnetID, spec := range intent.Subnets {
		switchName := topology.SubnetSwitch(subnetID)
		routerName := topology.VPCRouter(spec.VPCID)
		routerPortName := topology.SubnetRouterPort(subnetID)
		switchRouterPortName := topology.SubnetSwitchRouterPort(subnetID)

		gwIP, prefixBits, err := topology.SubnetGatewayCIDR(spec.CIDR)
		if err != nil {
			slog.Error("reconcile/apply: subnet gateway calc failed", "subnet_id", subnetID, "err", err)
			continue
		}
		gwCIDRString := fmt.Sprintf("%s/%d", gwIP, prefixBits)
		routerMAC := utils.HashMAC(subnetID)

		if _, ok := actual.Switches[switchName]; !ok {
			ls := &nbdb.LogicalSwitch{
				Name: switchName,
				ExternalIDs: map[string]string{
					"spinifex:subnet_id": subnetID,
					"spinifex:vpc_id":    spec.VPCID,
				},
			}
			if _, err := r.ovn.EnsureLogicalSwitch(ctx, ls); err != nil {
				slog.Error("reconcile/apply: ensure subnet switch failed", "subnet_id", subnetID, "err", err)
				continue
			}
			actual.Switches[switchName] = struct{}{}
		}

		if _, ok := actual.RouterPorts[routerPortName]; !ok {
			lrp := &nbdb.LogicalRouterPort{
				Name:     routerPortName,
				MAC:      routerMAC,
				Networks: []string{gwCIDRString},
				ExternalIDs: map[string]string{
					"spinifex:subnet_id": subnetID,
					"spinifex:vpc_id":    spec.VPCID,
				},
			}
			if err := r.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
				slog.Error("reconcile/apply: create subnet LRP failed", "subnet_id", subnetID, "err", err)
				continue
			}
			actual.RouterPorts[routerPortName] = struct{}{}
		}

		if existing, err := r.ovn.GetLogicalSwitchPort(ctx, switchRouterPortName); err != nil {
			lsp := &nbdb.LogicalSwitchPort{
				Name:      switchRouterPortName,
				Type:      "router",
				Addresses: []string{"router"},
				Options: map[string]string{
					"router-port": routerPortName,
					"arp_proxy":   handlers_imds.MetaDataServerIP,
				},
				ExternalIDs: map[string]string{
					"spinifex:subnet_id": subnetID,
					"spinifex:vpc_id":    spec.VPCID,
				},
			}
			if err := r.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
				slog.Error("reconcile/apply: create subnet router-LSP failed", "subnet_id", subnetID, "err", err)
				continue
			}
		} else if existing.Options["arp_proxy"] != handlers_imds.MetaDataServerIP {
			// Drift convergence: a subnet router LSP created before IMDS proxy-ARP
			// landed will not gain arp_proxy from a redeploy alone (the create
			// branch above only fires when the LSP is absent). Patch it in place so
			// existing subnets become IMDS-reachable for link-local guests.
			if existing.Options == nil {
				existing.Options = map[string]string{}
			}
			existing.Options["arp_proxy"] = handlers_imds.MetaDataServerIP
			if err := r.ovn.UpdateLogicalSwitchPort(ctx, existing); err != nil {
				slog.Error("reconcile/apply: patch subnet router-LSP arp_proxy failed", "subnet_id", subnetID, "err", err)
			} else {
				slog.Info("reconcile/apply: patched subnet router-LSP with IMDS arp_proxy", "subnet_id", subnetID, "port", switchRouterPortName)
			}
		}

		if existing, err := r.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", subnetID); err != nil || existing == nil {
			opts := &nbdb.DHCPOptions{
				CIDR:    spec.CIDR.String(),
				Options: topology.BuildSubnetDHCPOptions(gwIP, routerMAC, r.dnsServer),
				ExternalIDs: map[string]string{
					"spinifex:subnet_id": subnetID,
					"spinifex:vpc_id":    spec.VPCID,
				},
			}
			if _, dErr := r.ovn.CreateDHCPOptions(ctx, opts); dErr != nil {
				slog.Warn("reconcile/apply: create DHCP options failed (non-fatal)", "subnet_id", subnetID, "err", dErr)
			}
		}

		slog.Info("reconcile/apply: ensured subnet topology",
			"subnet_id", subnetID, "switch", switchName, "router_port", routerPortName)
	}
}

// applySGs ensures every intent SG has a port group, then (when
// pruneOrphans) deletes sg_* PGs without a matching intent SG.
// pruneOrphans=false is startup mode: a leader can load intent before peer
// subscribers have finished creating the PGs, so pruning at boot would
// delete in-flight resources. Drift always prunes.
func (r *reconciler) applySGs(ctx context.Context, intent IntentState, actual ActualState, pruneOrphans bool) {
	for groupID, spec := range intent.SGs {
		if err := r.topology.EnsureSGPortGroup(ctx, groupID); err != nil {
			slog.Error("reconcile/apply: EnsureSGPortGroup failed", "sg", groupID, "err", err)
			continue
		}
		actual.PortGroups[topology.SecurityGroupPortGroup(groupID)] = struct{}{}
		if err := r.sg.EnsureSG(ctx, spec); err != nil {
			slog.Error("reconcile/apply: EnsureSG failed", "sg", groupID, "err", err)
		}
	}

	if !pruneOrphans {
		return
	}

	wantPGs := make(map[string]struct{}, len(intent.SGs))
	for groupID := range intent.SGs {
		wantPGs[topology.SecurityGroupPortGroup(groupID)] = struct{}{}
	}
	for pgName := range actual.PortGroups {
		if !portGroupIsManaged(pgName) {
			continue
		}
		if _, ok := wantPGs[pgName]; ok {
			continue
		}
		if err := r.topology.DeleteSGPortGroupByName(ctx, pgName); err != nil {
			slog.Warn("reconcile/apply: orphan DeleteSGPortGroupByName failed", "pg", pgName, "err", err)
			continue
		}
		delete(actual.PortGroups, pgName)
		slog.Info("reconcile/apply: removed orphan port group", "pg", pgName)
	}
}

// applyPorts ensures each intent ENI has an LSP with PG memberships matching
// its SGIDs. Existing ports get a diff-based UpdatePortGroupMemberships so an
// SG swap never exposes an "unrestricted" gap (OVN default).
func (r *reconciler) applyPorts(ctx context.Context, intent IntentState, actual ActualState) {
	for portID, spec := range intent.Ports {
		portName := topology.Port(portID)
		switchName := topology.SubnetSwitch(spec.SubnetID)
		desiredPGs := make([]string, 0, len(spec.SGIDs))
		for _, sgID := range spec.SGIDs {
			pgName := topology.SecurityGroupPortGroup(sgID)
			if _, ok := actual.PortGroups[pgName]; !ok {
				slog.Warn("reconcile/apply: skipping port SG membership — port group missing in OVN",
					"port", portName, "sg", sgID, "pg", pgName)
				continue
			}
			desiredPGs = append(desiredPGs, pgName)
		}

		if _, err := r.ovn.GetLogicalSwitchPort(ctx, portName); err != nil {
			addrStr := spec.MAC.String() + " " + spec.PrivateIP.String()
			lsp := &nbdb.LogicalSwitchPort{
				Name:         portName,
				Addresses:    []string{addrStr},
				PortSecurity: []string{addrStr},
				ExternalIDs: map[string]string{
					"spinifex:eni_id":    portID,
					"spinifex:subnet_id": spec.SubnetID,
					"spinifex:vpc_id":    spec.VPCID,
				},
			}
			if dhcpOpts, derr := r.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", spec.SubnetID); derr == nil && dhcpOpts != nil {
				lsp.DHCPv4Options = &dhcpOpts.UUID
			}
			if err := r.ovn.CreateLogicalSwitchPortInGroups(ctx, switchName, lsp, desiredPGs); err != nil {
				slog.Error("reconcile/apply: create ENI port failed", "port", portName, "err", err)
			}
			continue
		}

		currentPGs, err := r.ovn.ListPortGroupsForPort(ctx, portName)
		if err != nil {
			slog.Warn("reconcile/apply: list port groups for port failed", "port", portName, "err", err)
			continue
		}
		addPGs, removePGs := diffSets(desiredPGs, currentPGs)
		if len(addPGs) == 0 && len(removePGs) == 0 {
			continue
		}
		if err := r.ovn.UpdatePortGroupMemberships(ctx, portName, addPGs, removePGs); err != nil {
			slog.Warn("reconcile/apply: update port group memberships failed", "port", portName, "err", err)
		}
	}
}

// applyIGWs ensures every intent IGW's OVN topology (external switch,
// localnet LSP, gateway LRP, default route, Gateway_Chassis bindings) and
// rebinds chassis on existing IGWs. AttachIGW is idempotent.
func (r *reconciler) applyIGWs(ctx context.Context, intent IntentState, actual ActualState) {
	for vpcID, spec := range intent.IGWs {
		if _, ok := actual.ExternalSwch[vpcID]; !ok {
			if err := r.igw.AttachIGW(ctx, spec); err != nil {
				slog.Error("reconcile/apply: AttachIGW failed", "vpc_id", vpcID, "err", err)
				continue
			}
			actual.ExternalSwch[vpcID] = struct{}{}
		}
		r.rebindGatewayChassis(ctx, vpcID)
	}
}

// rebindGatewayChassis re-asserts (chassis,priority) tuples on an existing
// gateway LRP. SetGatewayChassis is idempotent.
func (r *reconciler) rebindGatewayChassis(ctx context.Context, vpcID string) {
	if len(r.chassis) == 0 {
		return
	}
	gwPortName := topology.GatewayRouterPort(vpcID)
	if _, err := r.ovn.GetLogicalRouterPort(ctx, gwPortName); err != nil {
		return
	}
	for i, chassis := range r.chassis {
		priority := max(20-(i*5), 1)
		if err := r.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
			slog.Warn("reconcile/apply: SetGatewayChassis failed", "vpc_id", vpcID, "chassis", chassis, "err", err)
		}
	}
}

// applyEIPs runs every intent EIP through NATManager.AddEIP (idempotent;
// stale dnat_and_snat rules are cleaned first).
func (r *reconciler) applyEIPs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.EIPs {
		if err := r.nat.AddEIP(ctx, spec); err != nil {
			slog.Error("reconcile/apply: AddEIP failed", "external_ip", spec.ExternalIP, "logical_ip", spec.LogicalIP, "err", err)
		}
	}
}

// applyNATGWs runs every intent NAT gateway through NATManager.AddNATGateway.
// Duplicate (snat,SubnetCIDR) tuples are rejected by the underlying client.
func (r *reconciler) applyNATGWs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.NATGWs {
		if err := r.nat.AddNATGateway(ctx, spec); err != nil {
			slog.Warn("reconcile/apply: AddNATGateway failed (likely already exists)",
				"natgw_id", spec.NATGatewayID, "subnet_cidr", spec.SubnetCIDR, "err", err)
		}
	}
}

// diffSets returns (desired - current, current - desired).
func diffSets(desired, current []string) (add, remove []string) {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, s := range desired {
		desiredSet[s] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, s := range current {
		currentSet[s] = struct{}{}
	}
	for s := range desiredSet {
		if _, ok := currentSet[s]; !ok {
			add = append(add, s)
		}
	}
	for s := range currentSet {
		if _, ok := desiredSet[s]; !ok {
			remove = append(remove, s)
		}
	}
	return add, remove
}
