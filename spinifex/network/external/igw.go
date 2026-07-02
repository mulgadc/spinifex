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
	// EnsureSubnetEgress installs a Logical_Router_Policy rerouting traffic
	// from subnetID matching prefix via the IGW's gateway port. Idempotent.
	EnsureSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveSubnetEgress is the inverse. Idempotent.
	RemoveSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureNATGatewaySubnetEgress installs the NATGW egress policy at
	// SubnetEgressPriorityNATGW (lower than IGW). Requires AttachIGW first.
	EnsureNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveNATGatewaySubnetEgress is the inverse. Idempotent.
	RemoveNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureSubnetEgressDrop installs a DROP policy for subnets lacking a
	// 0.0.0.0/0 route table entry. Fires after lr_in_ip_routing. Idempotent.
	EnsureSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// RemoveSubnetEgressDrop is the inverse. Idempotent.
	RemoveSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
	// EnsureSystemInstanceEgress wires SNAT-only egress for a single instance
	// (instanceIP → externalIP). Installs a /32 reroute above the drop gate.
	// Requires AttachIGW first. Idempotent.
	EnsureSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error
	// RemoveSystemInstanceEgress is the inverse. Idempotent.
	RemoveSystemInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP, externalIP string) error
	// EnsureEIPInstanceEgress installs a /32 reroute above the drop gate for an
	// EIP-backed instance — reroute only, no SNAT. The EIP's dnat_and_snat already
	// SNATs the instance, so the reroute alone lets the inbound connection's reply
	// (and instance-initiated egress) bypass the subnet drop gate. Idempotent.
	EnsureEIPInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP string) error
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

// AttachIGW wires external connectivity for spec.VPCID onto the shared external
// switch: it ensures the singleton external switch + localnet, attaches the VPC
// gateway LRP via its own router-type switch port, installs the default route,
// binds gateway chassis, and waits on the flows barrier. Idempotent, and
// migration-safe: a VPC still bound to a legacy per-VPC switch re-attaches to the
// shared switch once the legacy switch is pruned.
func (m *igwManager) AttachIGW(ctx context.Context, spec IGWSpec) error {
	if spec.VPCID == "" {
		return errors.New("AttachIGW: VPCID required")
	}

	extSwitchName := topology.ExternalSwitchShared()
	extPortName := topology.ExternalLocalnetPortShared()
	gwPortName := topology.GatewayRouterPort(spec.VPCID)
	switchGWPortName := topology.GatewaySwitchPort(spec.VPCID)
	routerName := topology.VPCRouter(spec.VPCID)

	if err := m.ensureSharedExternal(ctx, extSwitchName, extPortName); err != nil {
		return err
	}

	// The gateway switch port on the shared switch is the per-VPC attach marker.
	if _, err := m.ovn.GetLogicalSwitchPort(ctx, switchGWPortName); err == nil {
		slog.Debug("external: IGW already attached for VPC, skipping",
			"vpc_id", spec.VPCID, "gw_port", switchGWPortName)
		return nil
	}

	gwNetwork, wanNexthop, gwLrpIP, err := m.resolveGatewayNetwork(ctx, spec.VPCID)
	if err != nil {
		return err
	}

	// The gateway LRP may already exist (e.g. migrating off a legacy per-VPC
	// switch); create it only when absent so its allocated IP is preserved.
	createdLRP := false
	if _, err := m.ovn.GetLogicalRouterPort(ctx, gwPortName); err != nil {
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
			return fmt.Errorf("create gateway router port %s: %w", gwPortName, err)
		}
		createdLRP = true
	}

	if err := m.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, &nbdb.LogicalSwitchPort{
		Name:      switchGWPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": gwPortName},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:igw_id": spec.InternetGatewayID,
			"spinifex:role":   "gateway",
		},
	}); err != nil {
		if createdLRP {
			_ = m.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName)
		}
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

// ensureSharedExternal idempotently creates the singleton shared external switch
// and its single localnet port. The localnet advertises router NAT addresses in
// centralized mode so gateways behind it are reachable from the uplink.
func (m *igwManager) ensureSharedExternal(ctx context.Context, switchName, portName string) error {
	extSwitch := &nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: map[string]string{"spinifex:role": "external"},
	}
	if _, _, err := m.ovn.EnsureLogicalSwitch(ctx, extSwitch); err != nil {
		return fmt.Errorf("ensure shared external switch %s: %w", switchName, err)
	}
	if _, err := m.ovn.GetLogicalSwitchPort(ctx, portName); err == nil {
		return nil
	}
	localnetOpts := map[string]string{"network_name": "external"}
	if m.natMode == policy.NATModeCentralized {
		localnetOpts["nat-addresses"] = "router"
	}
	if err := m.ovn.CreateLogicalSwitchPort(ctx, switchName, &nbdb.LogicalSwitchPort{
		Name:        portName,
		Type:        "localnet",
		Addresses:   []string{"unknown"},
		Options:     localnetOpts,
		ExternalIDs: map[string]string{"spinifex:role": "external-localnet"},
	}); err != nil {
		return fmt.Errorf("create shared localnet port %s: %w", portName, err)
	}
	return nil
}

// DetachIGW removes the VPC's gateway attachment — its switch port on the shared
// external switch, gateway LRP, default route, and SNAT — while preserving the
// shared external switch and localnet for other VPCs. Idempotent.
func (m *igwManager) DetachIGW(ctx context.Context, vpcID string) error {
	if vpcID == "" {
		return errors.New("DetachIGW: vpcID required")
	}

	extSwitchName := topology.ExternalSwitchShared()
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

	if err := m.allocator.Release(ctx, vpcID); err != nil {
		slog.Warn("external: allocator release failed", "vpc_id", vpcID, "err", err)
	}

	slog.Info("external: detached internet gateway", "vpc_id", vpcID)
	return nil
}

// EnsureSubnetEgress installs a per-subnet egress reroute policy on the VPC router.
// Idempotent.
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

// EnsureNATGatewaySubnetEgress installs the NATGW-priority egress policy
// (lower than IGW so IGW wins if both apply).
func (m *igwManager) EnsureNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	return m.ensureSubnetEgressAtPriority(ctx, vpcID, subnetID, prefix, policy.SubnetEgressPriorityNATGW, "EnsureNATGatewaySubnetEgress")
}

// RemoveNATGatewaySubnetEgress deletes the egress policy for (subnetID, prefix).
// Idempotent.
func (m *igwManager) RemoveNATGatewaySubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("RemoveNATGatewaySubnetEgress: vpcID and subnetID required")
	}
	return m.routes.DeleteSubnetEgress(ctx, vpcID, subnetID, prefix, m.rerouteExcludeCIDRs(ctx, vpcID))
}

// EnsureSubnetEgressDrop installs a drop policy for subnets lacking a 0.0.0.0/0
// route. Fires in lr_in_policy after routing; excludes VPC CIDR, link-local, multicast.
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

// EnsureSystemInstanceEgress installs a /32 reroute above the drop gate plus a plain
// snat (instanceIP → externalIP). No DNAT: the instance is not reachable inbound.
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

// EnsureEIPInstanceEgress installs the /32 reroute above the drop gate for an EIP
// instance, without the plain SNAT EnsureSystemInstanceEgress adds: the EIP's
// dnat_and_snat already SNATs the instance, so the reroute alone is what lets the
// inbound connection's reply bypass the subnet drop gate (lr_in_policy runs before
// lr_out un-DNAT/SNAT, so the reply still carries its private source at the gate).
// Idempotent.
func (m *igwManager) EnsureEIPInstanceEgress(ctx context.Context, vpcID, subnetID, instanceIP string) error {
	if vpcID == "" || subnetID == "" {
		return errors.New("EnsureEIPInstanceEgress: vpcID and subnetID required")
	}
	srcIP, err := netip.ParseAddr(instanceIP)
	if err != nil {
		return fmt.Errorf("EnsureEIPInstanceEgress: parse instance IP %q: %w", instanceIP, err)
	}
	_, nexthop, _, err := m.resolveGatewayNetwork(ctx, vpcID)
	if err != nil {
		return fmt.Errorf("resolve gateway network for %s: %w", vpcID, err)
	}
	if nexthop == "" {
		return fmt.Errorf("EnsureEIPInstanceEgress: no gateway nexthop for %s (IGW not attached?)", vpcID)
	}
	return m.routes.AddSystemInstanceEgress(ctx, vpcID, policy.SystemInstanceEgressSpec{
		SubnetID:     subnetID,
		SrcIP:        srcIP,
		Prefix:       defaultRoutePrefix,
		Nexthop:      nexthop,
		OutputPort:   topology.GatewayRouterPort(vpcID),
		ExcludeCIDRs: m.vpcExcludeCIDRs(ctx, vpcID),
	})
}

// linkLocalCIDR / multicastCIDR are appended to drop-policy excludes so link-local
// and multicast are not killed by the per-subnet drop gate.
var (
	linkLocalCIDR      = netip.MustParsePrefix("169.254.0.0/16")
	multicastCIDR      = netip.MustParsePrefix("224.0.0.0/4")
	defaultRoutePrefix = netip.MustParsePrefix("0.0.0.0/0")
)

// dropExcludeCIDRs returns the exclusion list for a drop policy: VPC CIDR,
// link-local, and multicast.
func (m *igwManager) dropExcludeCIDRs(ctx context.Context, vpcID string) []netip.Prefix {
	excludes := []netip.Prefix{linkLocalCIDR, multicastCIDR}
	if vpc := m.vpcExcludeCIDRs(ctx, vpcID); len(vpc) > 0 {
		excludes = append(vpc, excludes...)
	}
	return excludes
}

// rerouteExcludeCIDRs returns the exclusion list for an egress reroute policy:
// VPC CIDR plus link-local (0.0.0.0/0 would otherwise divert link-local out the gateway).
func (m *igwManager) rerouteExcludeCIDRs(ctx context.Context, vpcID string) []netip.Prefix {
	excludes := m.vpcExcludeCIDRs(ctx, vpcID)
	return append(excludes, linkLocalCIDR)
}

// vpcExcludeCIDRs returns the VPC's primary CIDR from the LR ExternalIDs.
// Returns nil on lookup failure; caller installs policy without exclusions.
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

// resolveGatewayNetwork resolves the LRP network/nexthop/IP for the VPC gateway port.
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
