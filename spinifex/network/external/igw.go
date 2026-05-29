package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
