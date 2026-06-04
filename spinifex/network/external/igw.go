package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// FlowsBarrier blocks until ovn-northd compiles NB→SB and chassis install flows.
// Production wraps `ovn-nbctl --wait=hv sync`; tests stub.
type FlowsBarrier func() error

// IGWManager attaches/detaches Internet Gateways to VPCs. Idempotent.
type IGWManager interface {
	AttachIGW(ctx context.Context, spec IGWSpec) error
	DetachIGW(ctx context.Context, vpcID string) error
	// EnsureSubnetEgress installs an OVN Logical_Router_Policy on the VPC
	// router so traffic sourced from subnetID and destined for prefix
	// reroutes via the IGW's gateway port. Called from the route-table
	// subscriber when CreateRoute / AssociateRouteTable wires a subnet to a
	// route table carrying a 0.0.0.0/0 -> igw-X route. Idempotent.
	EnsureSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveSubnetEgress is the inverse. Idempotent.
	RemoveSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureNATGatewaySubnetEgress installs the per-subnet egress policy that
	// gives private-subnet packets a default route to leave the LR after
	// NATGW SNAT. Reuses the IGW's gateway port + wan nexthop (NATGW SNAT
	// happens on the same VPC router); priority SubnetEgressPriorityNATGW
	// (lower than IGW so an IGW route on the same subnet would win).
	// Requires AttachIGW to have run first (NATGW depends on IGW per AWS).
	EnsureNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveNATGatewaySubnetEgress is the inverse. Idempotent.
	RemoveNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureSubnetEgressDrop installs a DROP policy at
	// SubnetEgressPriorityDrop for subnets whose effective route table lacks
	// a 0.0.0.0/0 entry. The drop fires after lr_in_ip_routing has matched
	// the VPC LR's router-wide default static route, killing the packet
	// before egress. Idempotent.
	EnsureSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveSubnetEgressDrop is the inverse. Idempotent.
	RemoveSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureSystemInstanceEgress wires egress-only internet access for a
	// single system instance (e.g. an EKS K3s server VM) sourced from
	// instanceIP in subnetID, SNAT'd to externalIP. Installs a /32 reroute
	// policy at SystemInstanceEgressPriority (above the drop gate so it works
	// even on a drop-gated subnet) plus a plain snat (no DNAT, no inbound).
	// Requires AttachIGW to have run first. Idempotent.
	EnsureSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error
	// RemoveSystemInstanceEgress is the inverse. Idempotent.
	RemoveSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error
}

// IGWManagerConfig is the construction-time bag for igwManager.
// FlowsBarrier defaults to a no-op when nil.
type IGWManagerConfig struct {
	OVN          ovn.Client
	Routes       policy.RouteManager
	NAT          policy.NATManager
	Pool         *ExternalPoolConfig
	Allocator    GatewayIPAllocator
	Chassis      []string
	NATMode      policy.NATMode
	FlowsBarrier FlowsBarrier
}

type igwManager struct {
	ovn       ovn.Client
	routes    policy.RouteManager
	nat       policy.NATManager
	pool      *ExternalPoolConfig
	allocator GatewayIPAllocator
	chassis   []string
	natMode   policy.NATMode
	barrier   FlowsBarrier
}

var _ IGWManager = (*igwManager)(nil)

// NewIGWManager constructs an IGWManager from cfg.
func NewIGWManager(cfg IGWManagerConfig) (IGWManager, error) {
	switch {
	case cfg.OVN == nil:
		return nil, errors.New("IGWManager: OVN client required")
	case cfg.Routes == nil:
		return nil, errors.New("IGWManager: RouteManager required")
	case cfg.NAT == nil:
		return nil, errors.New("IGWManager: NATManager required")
	case cfg.Allocator == nil:
		return nil, errors.New("IGWManager: GatewayIPAllocator required")
	case cfg.NATMode == policy.NATModeUnknown:
		return nil, errors.New("IGWManager: NATMode unknown; resolve from host.Wiring.UplinkMode()")
	}
	barrier := cfg.FlowsBarrier
	if barrier == nil {
		barrier = func() error { return nil }
	}
	return &igwManager{
		ovn:       cfg.OVN,
		routes:    cfg.Routes,
		nat:       cfg.NAT,
		pool:      cfg.Pool,
		allocator: cfg.Allocator,
		chassis:   cfg.Chassis,
		natMode:   cfg.NATMode,
		barrier:   barrier,
	}, nil
}

// AttachIGW wires external connectivity for spec.VPCID: external switch +
// localnet + gateway LRP + default route + chassis + flows barrier.
// Idempotent: returns nil if the external switch already exists.
func (m *igwManager) AttachIGW(ctx context.Context, spec IGWSpec) error {
	if spec.VPCID == "" {
		return errors.New("AttachIGW: VPCID required")
	}

	extSwitchName := topology.ExternalSwitch(spec.VPCID)
	extPortName := topology.ExternalLocalnetPort(spec.VPCID)
	gwPortName := topology.GatewayRouterPort(spec.VPCID)
	switchGWPortName := topology.GatewaySwitchPort(spec.VPCID)
	routerName := topology.VPCRouter(spec.VPCID)

	extSwitch := &nbdb.LogicalSwitch{
		Name: extSwitchName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:igw_id": spec.InternetGatewayID,
			"spinifex:role":   "external",
		},
	}
	existingSwitch, err := m.ovn.EnsureLogicalSwitch(ctx, extSwitch)
	if err != nil {
		return fmt.Errorf("ensure external switch %s: %w", extSwitchName, err)
	}
	if existingSwitch.UUID != extSwitch.UUID {
		slog.Debug("external: IGW topology already exists, skipping",
			"vpc_id", spec.VPCID, "ext_switch", extSwitchName)
		return nil
	}

	localnetOpts := map[string]string{"network_name": "external"}
	if m.natMode == policy.NATModeCentralized {
		localnetOpts["nat-addresses"] = "router"
	}
	if err := m.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, &nbdb.LogicalSwitchPort{
		Name:      extPortName,
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   localnetOpts,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:igw_id": spec.InternetGatewayID,
		},
	}); err != nil {
		_ = m.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create localnet port %s: %w", extPortName, err)
	}

	gwNetwork, wanNexthop, gwLrpIP, err := m.resolveGatewayNetwork(ctx, spec.VPCID)
	if err != nil {
		_ = m.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, extPortName)
		_ = m.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return err
	}

	lrpExtIDs := map[string]string{
		"spinifex:vpc_id": spec.VPCID,
		"spinifex:igw_id": spec.InternetGatewayID,
		"spinifex:role":   "gateway",
	}
	if gwLrpIP != "" {
		lrpExtIDs[gatewayIPExtIDKey] = gwLrpIP
	}
	if err := m.ovn.CreateLogicalRouterPort(ctx, routerName, &nbdb.LogicalRouterPort{
		Name:        gwPortName,
		MAC:         utils.HashMAC(gwPortName),
		Networks:    []string{gwNetwork},
		ExternalIDs: lrpExtIDs,
	}); err != nil {
		_ = m.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, extPortName)
		_ = m.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create gateway router port %s: %w", gwPortName, err)
	}

	if err := m.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, &nbdb.LogicalSwitchPort{
		Name:      switchGWPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": gwPortName},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:igw_id": spec.InternetGatewayID,
		},
	}); err != nil {
		_ = m.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName)
		_ = m.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, extPortName)
		_ = m.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create switch gateway port %s: %w", switchGWPortName, err)
	}

	if err := m.routes.AddDefaultRoute(ctx, spec.VPCID, wanNexthop, gwPortName); err != nil {
		return fmt.Errorf("add default route on %s: %w", routerName, err)
	}

	if len(m.chassis) == 0 {
		slog.Warn("external: no chassis configured — gateway port has no chassis binding, external traffic will not flow",
			"vpc_id", spec.VPCID, "gw_port", gwPortName)
	}
	for i, chassis := range m.chassis {
		priority := max(20-(i*5), 1)
		if err := m.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
			slog.Warn("external: failed to set gateway chassis",
				"gw_port", gwPortName, "chassis", chassis, "priority", priority, "err", err)
		}
	}

	slog.Info("external: attached internet gateway",
		"vpc_id", spec.VPCID,
		"igw_id", spec.InternetGatewayID,
		"ext_switch", extSwitchName,
		"gw_port", gwPortName,
		"lrp_network", gwNetwork,
		"wan_nexthop", wanNexthop,
		"chassis_count", len(m.chassis),
	)

	_ = m.barrier()
	return nil
}

// DetachIGW reverses AttachIGW. Idempotent.
func (m *igwManager) DetachIGW(ctx context.Context, vpcID string) error {
	if vpcID == "" {
		return errors.New("DetachIGW: vpcID required")
	}

	extSwitchName := topology.ExternalSwitch(vpcID)
	extPortName := topology.ExternalLocalnetPort(vpcID)
	gwPortName := topology.GatewayRouterPort(vpcID)
	switchGWPortName := topology.GatewaySwitchPort(vpcID)
	routerName := topology.VPCRouter(vpcID)

	if err := m.routes.DeleteDefaultRoute(ctx, vpcID); err != nil {
		slog.Warn("external: delete default route failed", "router", routerName, "err", err)
	}

	if router, err := m.ovn.GetLogicalRouter(ctx, routerName); err == nil {
		vpcCIDR := router.ExternalIDs["spinifex:cidr"]
		if vpcCIDR != "" {
			if err := m.nat.DeleteSNAT(ctx, vpcID, vpcCIDR); err != nil {
				slog.Warn("external: delete IGW SNAT failed", "router", routerName, "cidr", vpcCIDR, "err", err)
			}
		}
	} else {
		slog.Warn("external: get router for NAT cleanup failed", "router", routerName, "err", err)
	}

	if err := m.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, switchGWPortName); err != nil {
		slog.Warn("external: delete switch gateway port failed", "port", switchGWPortName, "err", err)
	}
	if err := m.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName); err != nil {
		slog.Warn("external: delete gateway router port failed", "port", gwPortName, "err", err)
	}
	if err := m.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, extPortName); err != nil {
		slog.Warn("external: delete localnet port failed", "port", extPortName, "err", err)
	}
	if err := m.ovn.DeleteLogicalSwitch(ctx, extSwitchName); err != nil {
		return fmt.Errorf("delete external switch %s: %w", extSwitchName, err)
	}

	if err := m.allocator.Release(ctx, vpcID); err != nil {
		slog.Warn("external: allocator release failed", "vpc_id", vpcID, "err", err)
	}

	slog.Info("external: detached internet gateway", "vpc_id", vpcID)
	return nil
}

// EnsureSubnetEgress installs a per-subnet egress policy on the VPC router
// rerouting (inport == "rtr-<subnetID>" && ip4.dst == prefix) via the IGW
// nexthop out the gateway LRP. Idempotent (drift-replace via the underlying
// policy.RouteManager).
func (m *igwManager) EnsureSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	return m.ensureSubnetEgressAtPriority(ctx, vpcID, subnetID, prefix, policy.SubnetEgressPriorityIGW, "EnsureSubnetEgress")
}

// RemoveSubnetEgress deletes the policy installed by EnsureSubnetEgress.
// Idempotent.
func (m *igwManager) RemoveSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("RemoveSubnetEgress: vpcID and subnetID required")
	}
	return m.routes.DeleteSubnetEgress(ctx, vpcID, subnetID, prefix, m.rerouteExcludeCIDRs(ctx, vpcID))
}

// EnsureNATGatewaySubnetEgress is the NATGW priority sibling of
// EnsureSubnetEgress. Same nexthop and output port; lower priority so an
// IGW route on the same subnet (if ever both present) wins.
func (m *igwManager) EnsureNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	return m.ensureSubnetEgressAtPriority(ctx, vpcID, subnetID, prefix, policy.SubnetEgressPriorityNATGW, "EnsureNATGatewaySubnetEgress")
}

// RemoveNATGatewaySubnetEgress deletes the per-subnet egress policy at any
// priority for (subnetID, prefix). policy.RouteManager.DeleteSubnetEgress
// already deletes both IGW and NATGW priorities defensively.
func (m *igwManager) RemoveNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("RemoveNATGatewaySubnetEgress: vpcID and subnetID required")
	}
	return m.routes.DeleteSubnetEgress(ctx, vpcID, subnetID, prefix, m.rerouteExcludeCIDRs(ctx, vpcID))
}

// EnsureSubnetEgressDrop installs a drop policy at SubnetEgressPriorityDrop
// for a subnet whose effective route table lacks a 0.0.0.0/0 entry. The drop
// fires in lr_in_policy AFTER the VPC LR's router-wide default static route
// has already matched in lr_in_ip_routing, killing the packet before egress.
// Excludes the VPC CIDR (intra-VPC traffic untouched), 169.254.0.0/16 (DHCP
// / link-local / metadata), and 224.0.0.0/4 (multicast).
func (m *igwManager) EnsureSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("EnsureSubnetEgressDrop: vpcID and subnetID required")
	}
	return m.routes.AddSubnetEgressDrop(ctx, vpcID, policy.SubnetEgressDropSpec{
		SubnetID:     subnetID,
		Prefix:       prefix,
		ExcludeCIDRs: m.dropExcludeCIDRs(ctx, vpcID),
	})
}

// RemoveSubnetEgressDrop deletes the policy installed by
// EnsureSubnetEgressDrop. Idempotent.
func (m *igwManager) RemoveSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("RemoveSubnetEgressDrop: vpcID and subnetID required")
	}
	return m.routes.DeleteSubnetEgressDrop(ctx, vpcID, subnetID, prefix, m.dropExcludeCIDRs(ctx, vpcID))
}

func (m *igwManager) ensureSubnetEgressAtPriority(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix, priority int, opName string) error {
	if vpcID == "" || subnetID == "" {
		return fmt.Errorf("%s: vpcID and subnetID required", opName)
	}
	_, nexthop, _, err := m.resolveGatewayNetwork(ctx, vpcID)
	if err != nil {
		return fmt.Errorf("resolve gateway network for %s: %w", vpcID, err)
	}
	if nexthop == "" {
		return fmt.Errorf("%s: no gateway nexthop available for %s (IGW not attached?)", opName, vpcID)
	}
	return m.routes.AddSubnetEgress(ctx, vpcID, policy.SubnetEgressSpec{
		SubnetID:     subnetID,
		Prefix:       prefix,
		Nexthop:      nexthop,
		OutputPort:   topology.GatewayRouterPort(vpcID),
		Priority:     priority,
		ExcludeCIDRs: m.rerouteExcludeCIDRs(ctx, vpcID),
	})
}

// EnsureSystemInstanceEgress installs an egress-only path for one system
// instance: a /32 reroute policy out the gateway port plus a plain snat
// (instanceIP -> externalIP). The reroute sits above the subnet drop gate so
// it works even where the instance's subnet is otherwise private; where the
// subnet already has a 1000 reroute it is harmless (same nexthop). The snat is
// not dnat_and_snat, so the instance is never reachable inbound.
func (m *igwManager) EnsureSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("EnsureSystemInstanceEgress: vpcID and subnetID required")
	}
	srcIP, err := netip.ParseAddr(instanceIP)
	if err != nil {
		return fmt.Errorf("EnsureSystemInstanceEgress: parse instance IP %q: %w", instanceIP, err)
	}
	if externalIP == "" {
		return errors.New("EnsureSystemInstanceEgress: externalIP required")
	}

	_, nexthop, _, err := m.resolveGatewayNetwork(ctx, vpcID)
	if err != nil {
		return fmt.Errorf("resolve gateway network for %s: %w", vpcID, err)
	}
	if nexthop == "" {
		return fmt.Errorf("EnsureSystemInstanceEgress: no gateway nexthop for %s (IGW not attached?)", vpcID)
	}

	if err := m.routes.AddSystemInstanceEgress(ctx, vpcID, policy.SystemInstanceEgressSpec{
		SubnetID:     subnetID,
		SrcIP:        srcIP,
		Prefix:       defaultRoutePrefix,
		Nexthop:      nexthop,
		OutputPort:   topology.GatewayRouterPort(vpcID),
		ExcludeCIDRs: m.vpcExcludeCIDRs(ctx, vpcID),
	}); err != nil {
		return fmt.Errorf("add system instance egress reroute: %w", err)
	}
	if err := m.nat.AddSystemInstanceSNAT(ctx, vpcID, instanceIP+"/32", externalIP); err != nil {
		return fmt.Errorf("add system instance egress snat: %w", err)
	}
	return nil
}

// RemoveSystemInstanceEgress deletes the policy + snat installed by
// EnsureSystemInstanceEgress. Idempotent.
func (m *igwManager) RemoveSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("RemoveSystemInstanceEgress: vpcID and subnetID required")
	}
	srcIP, err := netip.ParseAddr(instanceIP)
	if err != nil {
		return fmt.Errorf("RemoveSystemInstanceEgress: parse instance IP %q: %w", instanceIP, err)
	}
	var firstErr error
	if err := m.routes.DeleteSystemInstanceEgress(ctx, vpcID, subnetID, srcIP, defaultRoutePrefix, m.vpcExcludeCIDRs(ctx, vpcID)); err != nil {
		firstErr = fmt.Errorf("delete system instance egress reroute: %w", err)
	}
	if err := m.nat.DeleteSystemInstanceSNAT(ctx, vpcID, instanceIP+"/32"); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete system instance egress snat: %w", err)
	}
	return firstErr
}

// linkLocalCIDR / multicastCIDR are appended to drop-policy excludes so
// link-local (DHCP and friends — traffic that should never egress the gateway)
// and multicast destinations are not killed by the per-subnet gate.
var (
	linkLocalCIDR      = netip.MustParsePrefix("169.254.0.0/16")
	multicastCIDR      = netip.MustParsePrefix("224.0.0.0/4")
	defaultRoutePrefix = netip.MustParsePrefix("0.0.0.0/0")
)

// dropExcludeCIDRs returns the exclusion list for a per-subnet drop policy:
// VPC CIDR (if discoverable) plus link-local and multicast. The drop policy is
// a default-deny within the subnet's egress so it must spare any class the
// platform legitimately uses outside the VPC.
func (m *igwManager) dropExcludeCIDRs(ctx context.Context, vpcID string) []netip.Prefix {
	excludes := []netip.Prefix{linkLocalCIDR, multicastCIDR}
	if vpc := m.vpcExcludeCIDRs(ctx, vpcID); len(vpc) > 0 {
		excludes = append(vpc, excludes...)
	}
	return excludes
}

// rerouteExcludeCIDRs returns the exclusion list for a per-subnet egress
// reroute policy: VPC CIDR (if discoverable) plus link-local. The reroute
// matches 0.0.0.0/0 — the widest possible scope — so it would otherwise divert
// link-local (169.254.0.0/16) out the gateway, where it has no business going.
// Multicast is not excluded here (unlike the drop policy): a reroute only
// diverts traffic that already has somewhere to go, so leaking multicast to the
// gateway is harmless, whereas the drop policy would kill it outright.
func (m *igwManager) rerouteExcludeCIDRs(ctx context.Context, vpcID string) []netip.Prefix {
	excludes := m.vpcExcludeCIDRs(ctx, vpcID)
	return append(excludes, linkLocalCIDR)
}

// vpcExcludeCIDRs fetches the VPC's primary CIDR from the LR ExternalIDs
// label so per-subnet egress reroute policies skip in-VPC destinations.
// Returns nil on lookup failure or unset label — caller installs a policy
// without exclusions, preserving prior behaviour.
func (m *igwManager) vpcExcludeCIDRs(ctx context.Context, vpcID string) []netip.Prefix {
	router, err := m.ovn.GetLogicalRouter(ctx, topology.VPCRouter(vpcID))
	if err != nil || router == nil {
		return nil
	}
	cidr := router.ExternalIDs["spinifex:cidr"]
	if cidr == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil
	}
	return []netip.Prefix{prefix}
}

// resolveGatewayNetwork picks LRP Networks/nexthop/IP. Distributed: link-local.
// Centralised: WAN-subnet IP (LRP is on-wire).
func (m *igwManager) resolveGatewayNetwork(ctx context.Context, vpcID string) (network, nexthop, gwIP string, err error) {
	network = linkLocalGatewayNetwork
	nexthop = linkLocalGatewayNexthop

	if m.pool != nil && m.pool.Gateway != "" {
		nexthop = m.pool.Gateway
	}

	if m.natMode != policy.NATModeCentralized || m.pool == nil {
		return network, nexthop, "", nil
	}

	ip, prefix, allocNexthop, ok, allocErr := m.allocator.Allocate(ctx, vpcID, m.pool)
	if allocErr != nil {
		return "", "", "", fmt.Errorf("allocate gateway LRP IP: %w", allocErr)
	}
	if !ok {
		return network, nexthop, "", nil
	}
	if allocNexthop != "" {
		nexthop = allocNexthop
	}
	return fmt.Sprintf("%s/%d", ip, prefix), nexthop, ip, nil
}
