package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Bounds for the post-rebind SB chassis-claim wait. Package vars so tests can shorten.
var (
	gatewayClaimTimeout  = 30 * time.Second
	gatewayClaimInterval = 2 * time.Second
)

// Bounds for the post-claim datapath-reachability wait. Package vars so tests can shorten.
var (
	gatewayDatapathTimeout  = 30 * time.Second
	gatewayDatapathInterval = 2 * time.Second
)

// Bounds for the guest-port SB Port_Binding convergence wait. Long enough to span
// a guest's post-reboot boot: the tap is replumbed several seconds after the host
// reconcile starts, so the window must outlast that. Package vars so tests shorten.
var (
	guestPortDatapathTimeout  = 45 * time.Second
	guestPortDatapathInterval = 5 * time.Second
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
	}
}

// applySubnets ensures every intent subnet has a LogicalSwitch, LRP, router LSP,
// and DHCPOptions row. Each step is idempotent.
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

		if _, err := r.ovn.GetLogicalSwitchPort(ctx, switchRouterPortName); err != nil {
			lsp := &nbdb.LogicalSwitchPort{
				Name:      switchRouterPortName,
				Type:      "router",
				Addresses: []string{"router"},
				Options: map[string]string{
					"router-port": routerPortName,
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
		}

		// IMDS localport rides on the subnet switch; idempotent.
		if r.imds != nil {
			if _, err := r.imds.EnsureForSubnet(ctx, subnetID, spec.VPCID, spec.CIDR); err != nil {
				slog.Error("reconcile/apply: IMDS EnsureForSubnet failed", "subnet_id", subnetID, "err", err)
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

// applySGs ensures every intent SG has a port group; when pruneOrphans is true,
// deletes sg_* PGs with no matching intent SG. Startup passes false to avoid
// deleting in-flight resources before peer subscribers have converged.
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

// applyPorts ensures each intent ENI has an LSP with PG memberships matching its
// SGIDs. Existing ports use diff-based UpdatePortGroupMemberships to avoid gaps.
// When pruneOrphans is true, ENI LSPs with no matching intent ENI are torn down;
// startup passes false so in-flight ports survive until subscribers converge.
func (r *reconciler) applyPorts(ctx context.Context, intent IntentState, actual ActualState, pruneOrphans bool) {
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

	if !pruneOrphans {
		return
	}
	r.pruneOrphanPorts(ctx, intent)
}

// pruneOrphanPorts deletes guest LSPs whose spinifex:eni_id has no matching
// intent ENI, closing the create-only gap that leaks ports across instance
// terminate and host reinstall. DeletePort clears PG memberships then removes
// the LSP (composed cascade).
func (r *reconciler) pruneOrphanPorts(ctx context.Context, intent IntentState) {
	lsps, err := r.ovn.ListLogicalSwitchPorts(ctx)
	if err != nil {
		slog.Warn("reconcile/apply: list LSPs for orphan prune failed", "err", err)
		return
	}
	for i := range lsps {
		eniID := lsps[i].ExternalIDs["spinifex:eni_id"]
		if eniID == "" {
			continue
		}
		if _, ok := intent.Ports[eniID]; ok {
			continue
		}
		spec := topology.PortSpec{PortID: eniID, SubnetID: lsps[i].ExternalIDs["spinifex:subnet_id"]}
		if err := r.topology.DeletePort(ctx, spec); err != nil {
			slog.Warn("reconcile/apply: orphan ENI DeletePort failed", "port", lsps[i].Name, "err", err)
			continue
		}
		slog.Info("reconcile/apply: removed orphan ENI port", "port", lsps[i].Name, "eni_id", eniID)
	}
}

// applyIGWs ensures every intent IGW has OVN topology and rebinds chassis on
// existing IGWs. AttachIGW is idempotent.
func (r *reconciler) applyIGWs(ctx context.Context, intent IntentState, actual ActualState) {
	for vpcID, spec := range intent.IGWs {
		if _, ok := actual.ExternalSwch[vpcID]; !ok {
			if err := r.igw.AttachIGW(ctx, spec); err != nil {
				slog.Error("reconcile/apply: AttachIGW failed", "vpc_id", vpcID, "err", err)
				continue
			}
			actual.ExternalSwch[vpcID] = struct{}{}
		}
		r.rebindGatewayChassis(ctx, vpcID, eipProbeIP(intent, vpcID))
	}
}

// eipProbeIP returns an associated EIP's external IP for vpcID, or "" if the VPC
// has none. Used as the gateway datapath probe target: an EIP exercises the NAT
// pipeline + WAN uplink, unlike the gateway LRP IP which OVN answers natively.
// Any associated EIP suffices; map order is irrelevant.
func eipProbeIP(intent IntentState, vpcID string) string {
	for _, spec := range intent.EIPs {
		if spec.VPCID == vpcID && spec.ExternalIP != "" {
			return spec.ExternalIP
		}
	}
	return ""
}

// rebindGatewayChassis re-asserts chassis priority tuples on the gateway LRP.
func (r *reconciler) rebindGatewayChassis(ctx context.Context, vpcID, eipIP string) {
	if len(r.chassis) == 0 {
		return
	}
	gwPortName := topology.GatewayRouterPort(vpcID)
	lrp, err := r.ovn.GetLogicalRouterPort(ctx, gwPortName)
	if err != nil {
		return
	}
	for i, chassis := range r.chassis {
		priority := max(20-(i*5), 1)
		if err := r.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
			slog.Warn("reconcile/apply: SetGatewayChassis failed", "vpc_id", vpcID, "chassis", chassis, "err", err)
		}
	}
	r.ensureGatewayClaimed(ctx, topology.GatewayChassisRedirectPort(vpcID))
	r.ensureGatewayDatapath(ctx, vpcID, gatewayLRPIP(lrp), eipIP)
}

// gatewayLRPIP returns the bare IPv4 of the gateway router port, parsed from its
// first CIDR network (e.g. "192.168.1.241/23" -> "192.168.1.241"). Empty when the
// LRP is nil or carries no network, which makes the datapath gate a no-op.
func gatewayLRPIP(lrp *nbdb.LogicalRouterPort) string {
	if lrp == nil || len(lrp.Networks) == 0 {
		return ""
	}
	ip, _, ok := strings.Cut(lrp.Networks[0], "/")
	if !ok {
		return ""
	}
	return ip
}

// ensureGatewayDatapath verifies the external datapath actually forwards after the
// SB claim converges. A claimed chassisredirect binding is not proof the flows are
// installed: a boot race or a later ovn-controller restart can leave the WAN-glue
// veth admin-down or the EIP NAT flows stale, leaving every control-plane signal
// green while EIPs stay unreachable. Prefer probing an associated EIP — forcing an
// ARP of the dnat_and_snat external IP exercises the NAT pipeline + WAN uplink
// without a guest dependency, whereas the gateway LRP IP OVN answers natively and
// stays green even when the EIP datapath is dead. Fall back to the LRP IP when the
// VPC has no EIP. On a miss repair the uplink + recompute, then re-probe until a
// short deadline. No-op when no verifier is wired or no probe target resolved.
func (r *reconciler) ensureGatewayDatapath(ctx context.Context, vpcID, gwIP, eipIP string) {
	if r.gwClaim == nil || (gwIP == "" && eipIP == "") {
		return
	}
	target := eipIP
	if target == "" {
		target = gwIP
	}
	probe := func() (bool, error) {
		if eipIP != "" {
			return r.gwClaim.EIPReachable(ctx, eipIP)
		}
		return r.gwClaim.GatewayReachable(ctx, gwIP)
	}
	deadline := time.Now().Add(gatewayDatapathTimeout)
	repaired := false
	for {
		reachable, err := probe()
		if err != nil {
			slog.Warn("reconcile/apply: gateway datapath probe failed", "vpc_id", vpcID, "target", target, "err", err)
			return
		}
		if reachable {
			if repaired {
				slog.Info("reconcile/apply: gateway datapath recovered after uplink repair", "vpc_id", vpcID, "target", target)
			}
			return
		}
		if !repaired {
			slog.Warn("reconcile/apply: gateway datapath unreachable despite SB claim; repairing uplink + forcing recompute",
				"vpc_id", vpcID, "target", target)
			if err := r.gwClaim.RepairDatapath(ctx); err != nil {
				slog.Warn("reconcile/apply: gateway datapath repair failed", "vpc_id", vpcID, "target", target, "err", err)
			}
			repaired = true
		}
		if time.Now().After(deadline) {
			slog.Error("reconcile/apply: gateway datapath did not recover after uplink repair; external connectivity degraded",
				"vpc_id", vpcID, "target", target, "timeout", gatewayDatapathTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gatewayDatapathInterval):
		}
	}
}

// ensureGatewayClaimed polls the SB chassisredirect binding after SetGatewayChassis.
// An unclaimed binding after reboot makes floating IPs unreachable; nudges recompute
// once, then gives up. No-op when no verifier is wired.
func (r *reconciler) ensureGatewayClaimed(ctx context.Context, crPortName string) {
	if r.gwClaim == nil {
		return
	}
	deadline := time.Now().Add(gatewayClaimTimeout)
	nudged := false
	for {
		claimed, err := r.gwClaim.GatewayPortClaimed(ctx, crPortName)
		if err != nil {
			slog.Warn("reconcile/apply: gateway SB claim check failed", "port", crPortName, "err", err)
			return
		}
		if claimed {
			if nudged {
				slog.Info("reconcile/apply: gateway SB chassis claim converged after recompute", "port", crPortName)
			}
			return
		}
		if !nudged {
			slog.Warn("reconcile/apply: gateway SB binding unclaimed; nudging ovn-controller recompute", "port", crPortName)
			if err := r.gwClaim.NudgeRecompute(ctx); err != nil {
				slog.Warn("reconcile/apply: ovn-controller recompute nudge failed", "port", crPortName, "err", err)
			}
			nudged = true
		}
		if time.Now().After(deadline) {
			slog.Error("reconcile/apply: gateway SB chassis claim did not converge; floating IPs may be unreachable",
				"port", crPortName, "timeout", gatewayClaimTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gatewayClaimInterval):
		}
	}
}

// applyEIPs runs every intent EIP through NATManager.AddEIP; idempotent. After the
// DNAT row is in place it gates on the guest ENI's SB Port_Binding: AddEIP only
// proves the gateway-chassis flow exists, not the gatewayLRP->guest hop, so a guest
// whose port has not converged (e.g. just after a host reboot) stays dark while
// every other signal is green.
func (r *reconciler) applyEIPs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.EIPs {
		if err := r.nat.AddEIP(ctx, spec); err != nil {
			slog.Error("reconcile/apply: AddEIP failed", "external_ip", spec.ExternalIP, "logical_ip", spec.LogicalIP, "err", err)
		}
		r.ensureGuestPortDatapath(ctx, spec.VPCID, spec.PortName)
	}
}

// applyPublicInstanceEgress exempts every public-IP instance from its subnet egress
// drop gate. Public IPs come from two disjoint sources: auto-assigned and ELB
// addresses recorded on the ENI (intent.Ports) and user EIPs in the EIP bucket
// (intent.EIPs). Both need the same /32 reroute above the gate.
func (r *reconciler) applyPublicInstanceEgress(ctx context.Context, intent IntentState, _ ActualState) {
	for _, p := range intent.Ports {
		if p.PublicIP.IsValid() {
			r.ensureEIPEgressExemption(ctx, intent, p.VPCID, p.SubnetID, p.PrivateIP.String())
		}
	}
	for _, e := range intent.EIPs {
		r.ensureEIPEgressExemption(ctx, intent, e.VPCID, subnetIDForIP(intent.Subnets, e.LogicalIP), e.LogicalIP)
	}
}

// ensureEIPEgressExemption punches a public-IP instance through its subnet's egress
// drop gate. The drop gate (installed for an IGW-attached subnet with no 0.0.0.0/0
// route) drops the instance's WAN-bound traffic — including the reply leg of an
// inbound connection, since lr_in_policy runs before lr_out un-DNAT/SNAT so the reply
// still carries its private source at the gate. A /32 reroute above the gate restores
// the datapath; the instance's dnat_and_snat supplies SNAT. No-op when the VPC has no
// IGW (no drop gate) or the instance maps to no known subnet.
func (r *reconciler) ensureEIPEgressExemption(ctx context.Context, intent IntentState, vpcID, subnetID, instanceIP string) {
	if _, hasIGW := intent.IGWs[vpcID]; !hasIGW {
		return
	}
	if subnetID == "" {
		slog.Warn("reconcile/apply: public instance maps to no subnet; skipping egress exemption",
			"vpc_id", vpcID, "instance_ip", instanceIP)
		return
	}
	if err := r.igw.EnsureEIPInstanceEgress(ctx, vpcID, subnetID, instanceIP); err != nil {
		slog.Error("reconcile/apply: EnsureEIPInstanceEgress failed",
			"vpc_id", vpcID, "subnet_id", subnetID, "instance_ip", instanceIP, "err", err)
	}
}

// subnetIDForIP returns the SubnetID whose CIDR contains ip, or "" if none match.
func subnetIDForIP(subnets map[string]topology.SubnetSpec, ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	for _, s := range subnets {
		if s.CIDR.IsValid() && s.CIDR.Contains(addr) {
			return s.SubnetID
		}
	}
	return ""
}

// ensureGuestPortDatapath verifies the guest ENI behind an EIP is reachable on the
// ingress path. AddEIP installs the DNAT and primes the host neigh, but until the
// guest port's SB Port_Binding is up the gatewayLRP->guest flow is missing and the
// DNAT-translated packet blackholes. Probe the binding; on a miss force an
// ovn-controller recompute and re-probe until a deadline. Recompute on every miss,
// not once: post-reboot the guest tap is replumbed seconds after this runs, so a
// single early nudge fires before the port exists and never binds it. No-op when no
// verifier is wired or the EIP carries no guest port.
func (r *reconciler) ensureGuestPortDatapath(ctx context.Context, vpcID, lspName string) {
	if r.gwClaim == nil || lspName == "" {
		return
	}
	deadline := time.Now().Add(guestPortDatapathTimeout)
	nudged := false
	for {
		up, err := r.gwClaim.GuestPortUp(ctx, lspName)
		if err != nil {
			slog.Warn("reconcile/apply: guest port datapath probe failed", "vpc_id", vpcID, "lsp", lspName, "err", err)
			return
		}
		if up {
			if nudged {
				slog.Info("reconcile/apply: guest port datapath converged after recompute", "vpc_id", vpcID, "lsp", lspName)
			}
			return
		}
		slog.Warn("reconcile/apply: guest port SB binding not up; nudging ovn-controller recompute",
			"vpc_id", vpcID, "lsp", lspName)
		if err := r.gwClaim.NudgeRecompute(ctx); err != nil {
			slog.Warn("reconcile/apply: ovn-controller recompute nudge failed", "vpc_id", vpcID, "lsp", lspName, "err", err)
		}
		nudged = true
		if time.Now().After(deadline) {
			slog.Error("reconcile/apply: guest port datapath did not converge; EIP ingress may be unreachable",
				"vpc_id", vpcID, "lsp", lspName, "timeout", guestPortDatapathTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(guestPortDatapathInterval):
		}
	}
}

// applyNATGWs runs every intent NAT gateway through NATManager.AddNATGateway.
func (r *reconciler) applyNATGWs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.NATGWs {
		if err := r.nat.AddNATGateway(ctx, spec); err != nil {
			slog.Warn("reconcile/apply: AddNATGateway failed (likely already exists)",
				"natgw_id", spec.NATGatewayID, "subnet_cidr", spec.SubnetCIDR, "err", err)
		}
	}
}

// applyIGWRoutes installs per-subnet egress reroute policies from intent.
// Closes the bootstrap race: events fire before subscribers attach; KV retains the route.
func (r *reconciler) applyIGWRoutes(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.IGWRoutes {
		if err := r.igw.EnsureSubnetEgress(ctx, spec.VPCID, spec.SubnetID, spec.DestCIDR); err != nil {
			slog.Error("reconcile/apply: EnsureSubnetEgress failed",
				"vpc_id", spec.VPCID, "subnet_id", spec.SubnetID, "cidr", spec.DestCIDR.String(), "err", err)
		}
	}
}

// applyNATGWRoutes is the NATGW priority sibling of applyIGWRoutes.
func (r *reconciler) applyNATGWRoutes(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.NATGWRoutes {
		if err := r.igw.EnsureNATGatewaySubnetEgress(ctx, spec.VPCID, spec.SubnetID, spec.DestCIDR); err != nil {
			slog.Error("reconcile/apply: EnsureNATGatewaySubnetEgress failed",
				"vpc_id", spec.VPCID, "subnet_id", spec.SubnetID, "cidr", spec.DestCIDR.String(), "err", err)
		}
	}
}

// applyDropGates installs DROP policies for subnets with an attached IGW but
// no 0.0.0.0/0 route, preventing unintended egress via the VPC default route.
func (r *reconciler) applyDropGates(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.DropGates {
		if err := r.igw.EnsureSubnetEgressDrop(ctx, spec.VPCID, spec.SubnetID, spec.DestCIDR); err != nil {
			slog.Error("reconcile/apply: EnsureSubnetEgressDrop failed",
				"vpc_id", spec.VPCID, "subnet_id", spec.SubnetID, "cidr", spec.DestCIDR.String(), "err", err)
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
