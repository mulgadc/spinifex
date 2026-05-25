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
)

// FlowsBarrier blocks until ovn-northd has compiled NB → SB and every
// chassis has installed the resulting flows. Injected so tests can stub.
// Production wiring passes a closure over `ovn-nbctl --wait=hv sync`.
type FlowsBarrier func() error

// IGWManager attaches and detaches Internet Gateways to VPCs. AttachIGW
// builds the external switch + localnet + gateway LRP + default route,
// schedules gateway chassis for HA, and waits for flows to land on every
// hypervisor. DetachIGW reverses the sequence and releases any allocator-
// held gateway IP.
//
// All operations are idempotent: re-attach is a no-op if the external
// switch already exists; re-detach succeeds even when objects are absent.
type IGWManager interface {
	AttachIGW(ctx context.Context, spec IGWSpec) error
	DetachIGW(ctx context.Context, vpcID string) error
}

// IGWManagerConfig is the construction-time bag for igwManager. All fields
// except FlowsBarrier are required; FlowsBarrier defaults to a no-op when
// nil (tests skip the wait, production wiring injects the real barrier).
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

// NewIGWManager constructs an IGWManager from cfg. Returns an error when
// required fields are missing or when NATMode is unknown.
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

// AttachIGW wires external connectivity for spec.VPCID. Layout:
//
//  1. External LogicalSwitch (ext-{vpcID})
//  2. Localnet LSP on it (ext-port-{vpcID}) — nat-addresses=router only in
//     centralised NAT mode.
//  3. Gateway LRP on the VPC router (gw-{vpcID}) — IP from allocator or
//     link-local fallback.
//  4. Switch-side router peer LSP (gw-port-{vpcID}).
//  5. Default route 0.0.0.0/0 via WAN nexthop, OutputPort pinned to the
//     gateway LRP so northd doesn't drop it (link-local network doesn't
//     contain the nexthop).
//  6. SetGatewayChassis for every configured chassis, descending priority.
//  7. waitForFlowsHV barrier so the caller's reply is only sent once the
//     datapath is hot (mulga-siv-105 — without this, a follow-up vpc.add-nat
//     can complete on a dark datapath and the VM is unreachable until flows
//     install).
//
// First-line idempotency check: if the external switch already exists,
// returns nil without re-running any of the steps.
func (m *igwManager) AttachIGW(ctx context.Context, spec IGWSpec) error {
	if spec.VPCID == "" {
		return errors.New("AttachIGW: VPCID required")
	}

	extSwitchName := topology.ExternalSwitch(spec.VPCID)
	extPortName := topology.ExternalLocalnetPort(spec.VPCID)
	gwPortName := topology.GatewayRouterPort(spec.VPCID)
	switchGWPortName := "gw-port-" + spec.VPCID
	routerName := topology.VPCRouter(spec.VPCID)

	if _, err := m.ovn.GetLogicalSwitch(ctx, extSwitchName); err == nil {
		slog.Debug("external: IGW topology already exists, skipping",
			"vpc_id", spec.VPCID, "ext_switch", extSwitchName)
		return nil
	}

	if err := m.ovn.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name: extSwitchName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:igw_id": spec.InternetGatewayID,
			"spinifex:role":   "external",
		},
	}); err != nil {
		return fmt.Errorf("create external switch %s: %w", extSwitchName, err)
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
		MAC:         generateMAC(gwPortName),
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

// DetachIGW removes everything AttachIGW built for vpcID, in reverse
// order, treating each "already absent" as success.
func (m *igwManager) DetachIGW(ctx context.Context, vpcID string) error {
	if vpcID == "" {
		return errors.New("DetachIGW: vpcID required")
	}

	extSwitchName := topology.ExternalSwitch(vpcID)
	extPortName := topology.ExternalLocalnetPort(vpcID)
	gwPortName := topology.GatewayRouterPort(vpcID)
	switchGWPortName := "gw-port-" + vpcID
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

// resolveGatewayNetwork chooses the Networks CIDR + WAN nexthop + gw IP
// for the gateway LRP. Distributed mode always uses link-local (per-VM
// dnat_and_snat handles ARP per chassis). Centralised mode requires a
// WAN-subnet IP because the LRP is the on-wire egress point.
func (m *igwManager) resolveGatewayNetwork(ctx context.Context, vpcID string) (network, nexthop, gwIP string, err error) {
	network = linkLocalGatewayNetwork
	nexthop = linkLocalGatewayNexthop

	if m.pool != nil && m.pool.Gateway != "" {
		nexthop = m.pool.Gateway
	}

	if m.natMode != policy.NATModeCentralized || m.pool == nil {
		return network, nexthop, "", nil
	}

	ip, prefix, ok, allocErr := m.allocator.Allocate(ctx, vpcID, m.pool)
	if allocErr != nil {
		return "", "", "", fmt.Errorf("allocate gateway LRP IP: %w", allocErr)
	}
	if !ok {
		return network, nexthop, "", nil
	}
	return fmt.Sprintf("%s/%d", ip, prefix), nexthop, ip, nil
}
