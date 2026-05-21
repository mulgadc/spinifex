package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// applyVPCs ensures every intent VPC has a LogicalRouter. OVN-only routers
// are left alone (manual ops territory; tightening this is a Phase 4
// cleanup item).
func (r *reconciler) applyVPCs(ctx context.Context, intent IntentState, actual ActualState) {
	for vpcID, spec := range intent.VPCs {
		routerName := topology.VPCRouter(vpcID)
		if _, ok := actual.Routers[routerName]; ok {
			continue
		}
		lr := &nbdb.LogicalRouter{
			Name: routerName,
			ExternalIDs: map[string]string{
				"spinifex:vpc_id": vpcID,
				"spinifex:cidr":   spec.CIDR.String(),
			},
		}
		if err := r.ovn.CreateLogicalRouter(ctx, lr); err != nil {
			slog.Error("reconcile/apply: create VPC router failed", "vpc_id", vpcID, "err", err)
			continue
		}
		actual.Routers[routerName] = struct{}{}
		slog.Info("reconcile/apply: created VPC router", "vpc_id", vpcID, "router", routerName)
	}
}

// applySubnets ensures every intent Subnet has a LogicalSwitch, the matching
// LRP on the parent router, the router-side LSP on the switch, and a
// DHCPOptions row. Each of the four steps is independently idempotent so a
// partial existing topology heals without needing rollback.
func (r *reconciler) applySubnets(ctx context.Context, intent IntentState, actual ActualState) {
	for subnetID, spec := range intent.Subnets {
		switchName := topology.SubnetSwitch(subnetID)
		routerName := topology.VPCRouter(spec.VPCID)
		routerPortName := topology.SubnetRouterPort(subnetID)
		switchRouterPortName := topology.SubnetSwitchRouterPort(subnetID)

		gwIP, prefixBits, err := subnetGatewayCIDR(spec.CIDR)
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
			if err := r.ovn.CreateLogicalSwitch(ctx, ls); err != nil {
				slog.Error("reconcile/apply: create subnet switch failed", "subnet_id", subnetID, "err", err)
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
				Options:   map[string]string{"router-port": routerPortName},
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

		if existing, err := r.ovn.FindDHCPOptionsByCIDR(ctx, spec.CIDR.String()); err != nil || existing == nil {
			opts := &nbdb.DHCPOptions{
				CIDR: spec.CIDR.String(),
				Options: map[string]string{
					"server_id":  gwIP,
					"server_mac": routerMAC,
					"lease_time": "3600",
					"router":     gwIP,
					"dns_server": "8.8.8.8",
					"mtu":        "1442",
				},
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

// applySGs runs the SG stage in two halves: first ensure every intent SG's
// port group exists in OVN, then push the ACL set via the policy manager.
// Orphan port groups (sg_* with no matching intent SG) are torn down.
// Port group lifecycle remains the reconciler's responsibility until
// topology.Manager grows EnsureSecurityGroupPortGroup / DeleteSGPortGroup
// (parent plan §2.1 deferred to a follow-on bead).
func (r *reconciler) applySGs(ctx context.Context, intent IntentState, actual ActualState) {
	for groupID, spec := range intent.SGs {
		pgName := topology.SecurityGroupPortGroup(groupID)
		if _, ok := actual.PortGroups[pgName]; !ok {
			if err := r.ovn.CreatePortGroup(ctx, pgName, nil); err != nil {
				slog.Error("reconcile/apply: create SG port group failed", "sg", groupID, "pg", pgName, "err", err)
				continue
			}
			actual.PortGroups[pgName] = struct{}{}
		}
		if err := r.sg.EnsureSG(ctx, spec); err != nil {
			slog.Error("reconcile/apply: EnsureSG failed", "sg", groupID, "err", err)
		}
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
		if err := r.ovn.ClearACLs(ctx, pgName); err != nil {
			slog.Warn("reconcile/apply: orphan ClearACLs failed", "pg", pgName, "err", err)
			continue
		}
		if err := r.ovn.DeletePortGroup(ctx, pgName); err != nil {
			slog.Warn("reconcile/apply: orphan DeletePortGroup failed", "pg", pgName, "err", err)
			continue
		}
		delete(actual.PortGroups, pgName)
		slog.Info("reconcile/apply: removed orphan port group", "pg", pgName)
	}
}

// applyPorts ensures every intent ENI port has a LogicalSwitchPort with
// atomic port-group memberships matching its SGIDs. Existing ports get a
// diff-based UpdatePortGroupMemberships call so a 5-SG → different-5-SG
// modify never exposes an intermediate state (OVN default = unrestricted
// would apply for the gap). The atomic create-with-groups primitive is the
// reason this stage sits below applySGs in the topo order: the port groups
// must exist first.
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

// applyIGWs rebinds Gateway_Chassis on every intent IGW whose OVN topology
// already exists. Fresh-IGW creation flows through the NATS handler path
// (handlers/ec2/igw → vpc.igw-attach event → TopologyHandler.reconcileIGW)
// for the duration of Phase 2 — that path retains DHCP-uplink awareness
// via dhcpManager, which network/external.NewIGWManager does not yet have
// (bead mulga-siv-125.3.3 will remove the DHCP manager entirely; Phase 4
// then migrates the handler onto external.IGWManager).
//
// Chassis-rebind drift handling stays in the reconciler because it's a
// pure NB-DB-only fix-up: a system-id change across reboot leaves
// Gateway_Chassis rows pointing at a chassis no ovn-controller owns
// (mulga-999), and SetGatewayChassis is idempotent.
//
// The igw manager field stays wired (config requires it) so this stage
// can grow into a full AttachIGW path once DHCP awareness lands in
// network/external.
func (r *reconciler) applyIGWs(ctx context.Context, intent IntentState, actual ActualState) {
	for vpcID := range intent.IGWs {
		if _, ok := actual.ExternalSwch[vpcID]; !ok {
			slog.Debug("reconcile/apply: IGW topology missing in OVN, deferring to NATS handler path", "vpc_id", vpcID)
			continue
		}
		r.rebindGatewayChassis(ctx, vpcID)
	}
}

// rebindGatewayChassis is the chassis-drift correction step for an
// already-existing IGW gateway LRP. It calls SetGatewayChassis for every
// configured chassis; the OVN client's idempotency layer treats unchanged
// (chassis_name, priority) tuples as no-ops. Missing chassis are skipped
// at the caller level — we don't enumerate stale Gateway_Chassis rows
// here because IGWManager.AttachIGW owns that for fresh creates and the
// legacy reconcileGatewayChassis pass is going away in 2.4 when
// topology.go is split.
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

// applyEIPs runs every intent EIP through NATManager.AddEIP. AddEIP is
// idempotent (stale dnat_and_snat rules on any router referencing the
// external IP are cleaned before the new rule is added), so re-running on
// a hot reconciler boot just re-asserts the existing OVN state.
func (r *reconciler) applyEIPs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.EIPs {
		if err := r.nat.AddEIP(ctx, spec); err != nil {
			slog.Error("reconcile/apply: AddEIP failed", "external_ip", spec.ExternalIP, "logical_ip", spec.LogicalIP, "err", err)
		}
	}
}

// applyNATGWs runs every intent NAT gateway through NATManager.AddNATGateway.
// AddNATGateway rejects duplicate (snat, SubnetCIDR) tuples; on a re-run we
// rely on the underlying OVN client's existence check rather than a Get-first
// query here.
func (r *reconciler) applyNATGWs(ctx context.Context, intent IntentState, _ ActualState) {
	for _, spec := range intent.NATGWs {
		if err := r.nat.AddNATGateway(ctx, spec); err != nil {
			slog.Warn("reconcile/apply: AddNATGateway failed (likely already exists)",
				"natgw_id", spec.NATGatewayID, "subnet_cidr", spec.SubnetCIDR, "err", err)
		}
	}
}

// subnetGatewayCIDR returns the .1 host of the subnet's CIDR and the
// prefix bit count. Matches the legacy services/vpcd/topology.go's
// subnetGateway, kept local to the reconcile package so the cross-package
// dependency on services/vpcd can be dropped.
func subnetGatewayCIDR(prefix netip.Prefix) (string, int, error) {
	if !prefix.IsValid() {
		return "", 0, fmt.Errorf("invalid prefix")
	}
	addr := prefix.Masked().Addr()
	if !addr.Is4() {
		return "", 0, fmt.Errorf("only IPv4 supported: %s", prefix)
	}
	bytes := addr.As4()
	bytes[3]++
	return netip.AddrFrom4(bytes).String(), prefix.Bits(), nil
}

// diffSets returns the (desired - current) and (current - desired) sets.
// Used by applyPorts to compute the minimal port-group membership delta.
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
