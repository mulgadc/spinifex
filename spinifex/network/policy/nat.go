package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// EIPSpec is a 1:1 EIP NAT (dnat_and_snat). In NATModeDistributed PortName
// and MAC MUST be set for per-chassis flows; missing values fall back to
// centralised shape (hairpins via gateway chassis).
type EIPSpec struct {
	VPCID      string
	ExternalIP string
	LogicalIP  string
	PortName   string
	MAC        string
}

// NATGWSpec is a NAT Gateway SNAT rule keyed by (snat, SubnetCIDR).
type NATGWSpec struct {
	VPCID        string
	NATGatewayID string
	PublicIP     string
	SubnetCIDR   string
}

// NATManager owns NAT-rule lifecycle. Mode is fixed at construction.
type NATManager interface {
	// AddEIP installs a dnat_and_snat rule, cleaning stale rules for the
	// same ExternalIP on any router first (pool reuse across VPCs).
	AddEIP(ctx context.Context, eip EIPSpec) error

	// DeleteEIP removes the rule by LogicalIP and flushes the host ARP entry
	// for ExternalIP so a recycled IP is not shadowed by the stale MAC;
	// idempotent.
	DeleteEIP(ctx context.Context, vpcID, externalIP, logicalIP string) error

	// AddNATGateway installs the (snat, SubnetCIDR) rule; rejects overlap.
	AddNATGateway(ctx context.Context, gw NATGWSpec) error

	// DeleteNATGateway removes the (snat, SubnetCIDR) rule; idempotent.
	DeleteNATGateway(ctx context.Context, vpcID, subnetCIDR string) error

	// AddSNAT installs the IGW default-outbound snat rewriting VPC CIDR
	// to ExternalIP. AWS-parity callers typically skip this.
	AddSNAT(ctx context.Context, vpcID, vpcCIDR, externalIP string) error

	// DeleteSNAT removes the IGW default-outbound snat for vpcCIDR.
	DeleteSNAT(ctx context.Context, vpcID, vpcCIDR string) error

	// AddSystemInstanceSNAT installs an egress-only snat for a /32 logicalIP →
	// externalIP. Plain snat (not dnat_and_snat), so no inbound path. Idempotent.
	AddSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP, externalIP string) error

	// DeleteSystemInstanceSNAT removes the egress-only snat by logicalIP;
	// idempotent.
	DeleteSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP string) error
}

// FlowsBarrier blocks until every chassis has installed flows. Production
// wires `ovn-nbctl --wait=hv sync`; tests leave it nil.
type FlowsBarrier func() error

// NeighFlusher invalidates the host ARP entry for externalIP so a recycled
// external IP isn't shadowed by the prior owner's MAC (kernel ARP timeout 60-300s).
// Best-effort: callers warn and proceed on error.
type NeighFlusher func(externalIP string) error

// NeighPrimer programs the host ARP entry for a distributed EIP directly.
// Preferred over NeighFlusher when the MAC is known: a flush triggers re-ARP that
// no node answers for a same-chassis recycled IP. Best-effort; callers warn and proceed.
type NeighPrimer func(eip EIPSpec) error

type Option func(*natManager)

// WithFlowsBarrier injects the post-write flow-install barrier so callers
// only see success once every chassis has the rewrite flow.
func WithFlowsBarrier(b FlowsBarrier) Option {
	return func(m *natManager) {
		if b != nil {
			m.barrier = b
		}
	}
}

// WithNeighFlusher injects the ARP-flush hook fired on EIP detach and on attach
// when external_mac is unknown.
func WithNeighFlusher(f NeighFlusher) Option {
	return func(m *natManager) {
		if f != nil {
			m.neigh = f
		}
	}
}

// WithNeighPrimer injects the ARP-prime hook fired on EIP attach in distributed
// mode; preferred over the flusher when external_mac is known.
func WithNeighPrimer(p NeighPrimer) Option {
	return func(m *natManager) {
		if p != nil {
			m.neighPrime = p
		}
	}
}

type natManager struct {
	ovn        ovn.Client
	mode       NATMode
	barrier    FlowsBarrier
	neigh      NeighFlusher
	neighPrime NeighPrimer
}

var _ NATManager = (*natManager)(nil)

// NewNATManager constructs a NATManager bound to one NAT mode (must not be
// NATModeUnknown).
func NewNATManager(client ovn.Client, mode NATMode, opts ...Option) (NATManager, error) {
	if mode == NATModeUnknown {
		return nil, fmt.Errorf("NAT mode is unknown; resolve from host.Wiring.UplinkMode()")
	}
	m := &natManager{
		ovn:        client,
		mode:       mode,
		barrier:    func() error { return nil },
		neigh:      func(string) error { return nil },
		neighPrime: func(EIPSpec) error { return nil },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

func (m *natManager) AddEIP(ctx context.Context, eip EIPSpec) error {
	router := topology.VPCRouter(eip.VPCID)

	natRule := &nbdb.NAT{
		Type:       "dnat_and_snat",
		ExternalIP: eip.ExternalIP,
		LogicalIP:  eip.LogicalIP,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":    eip.VPCID,
			"spinifex:public_ip": eip.ExternalIP,
		},
	}
	distributed := m.mode == NATModeDistributed && eip.PortName != "" && eip.MAC != ""
	if distributed {
		mac := eip.MAC
		port := eip.PortName
		natRule.ExternalMAC = &mac
		natRule.LogicalPort = &port
	}

	// Skip when the existing row already matches; avoids the
	// delete-then-add flow-install gap on duplicate publishes.
	if existing, err := m.ovn.FindNATByExternalIP(ctx, "dnat_and_snat", eip.ExternalIP); err != nil {
		slog.Warn("policy: AddEIP idempotency lookup failed", "external_ip", eip.ExternalIP, "err", err)
	} else if existing != nil && existing.LogicalIP == eip.LogicalIP &&
		existing.ExternalIDs["spinifex:vpc_id"] == eip.VPCID &&
		(!distributed ||
			(existing.ExternalMAC != nil && *existing.ExternalMAC == eip.MAC &&
				existing.LogicalPort != nil && *existing.LogicalPort == eip.PortName)) {
		slog.Info("policy: AddEIP idempotent skip — rule current, re-priming reachability",
			"router", router, "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP)
		// Skip row churn but still re-prime: stop->start re-attaches the same EIP
		// and the host neigh stays dark until ARP times out without a fresh prime.
		m.primeReachability(ctx, eip, distributed)
		return nil
	}

	// Search every router for stale rules — vpc.delete-nat is fire-and-forget.
	if removed, err := m.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", eip.ExternalIP); err != nil {
		slog.Warn("policy: stale NAT cleanup failed before AddEIP", "external_ip", eip.ExternalIP, "err", err)
	} else if removed > 0 {
		slog.Info("policy: cleaned stale dnat_and_snat rules before AddEIP", "external_ip", eip.ExternalIP, "removed", removed)
	}

	if err := m.ovn.AddNAT(ctx, router, natRule); err != nil {
		return fmt.Errorf("add dnat_and_snat %s -> %s on %s: %w", eip.LogicalIP, eip.ExternalIP, router, err)
	}
	if err := m.barrier(); err != nil {
		slog.Warn("policy: AddEIP flows barrier failed", "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP, "err", err)
	}
	m.primeReachability(ctx, eip, distributed)
	return nil
}

// primeReachability programs the host neigh entry to the MAC owning the EIP on
// the WAN segment — external_mac (distributed) or gateway router-port MAC
// (centralised) — falling back to an ARP flush when no MAC resolves.
func (m *natManager) primeReachability(ctx context.Context, eip EIPSpec, distributed bool) {
	primeMAC := eip.MAC
	if !distributed {
		primeMAC = m.gatewayPortMAC(ctx, eip.VPCID)
	}
	if primeMAC != "" {
		primed := eip
		primed.MAC = primeMAC
		if err := m.neighPrime(primed); err != nil {
			slog.Warn("policy: AddEIP neighbour prime failed", "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP, "err", err)
		}
		return
	}
	if err := m.neigh(eip.ExternalIP); err != nil {
		slog.Warn("policy: AddEIP neighbour flush failed", "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP, "err", err)
	}
}

// gatewayPortMAC returns the MAC of the VPC gateway router's external port, the
// L2 owner of a centralised EIP on the WAN segment. Empty on lookup miss so the
// caller falls back to an ARP flush.
func (m *natManager) gatewayPortMAC(ctx context.Context, vpcID string) string {
	lrp, err := m.ovn.GetLogicalRouterPort(ctx, topology.GatewayRouterPort(vpcID))
	if err != nil || lrp == nil {
		return ""
	}
	return lrp.MAC
}

func (m *natManager) DeleteEIP(ctx context.Context, vpcID, externalIP, logicalIP string) error {
	router := topology.VPCRouter(vpcID)
	// Delete the dnat_and_snat row by its external IP, not its logical IP. A row's
	// identity on the gateway router is the EIP; private IPs are recycled as instances
	// come and go, so a stale or retried delete keyed on logical IP clobbers whichever
	// EIP currently holds that private IP (the next owner goes dark). An empty external
	// IP means there is no EIP and therefore no dnat_and_snat row to remove.
	if externalIP == "" {
		return nil
	}
	if err := m.ovn.DeleteNATByExternalIP(ctx, router, "dnat_and_snat", externalIP); err != nil {
		if !errors.Is(err, ovn.ErrNATNotFound) {
			return fmt.Errorf("delete dnat_and_snat %s on %s: %w", externalIP, router, err)
		}
	}
	// Flush host ARP for the released IP so the next owner isn't shadowed. Best-effort.
	if err := m.neigh(externalIP); err != nil {
		slog.Warn("policy: DeleteEIP neighbour flush failed", "external_ip", externalIP, "logical_ip", logicalIP, "err", err)
	}
	return nil
}

func (m *natManager) AddNATGateway(ctx context.Context, gw NATGWSpec) error {
	router := topology.VPCRouter(gw.VPCID)

	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: gw.PublicIP,
		LogicalIP:  gw.SubnetCIDR,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":         gw.VPCID,
			"spinifex:nat_gateway_id": gw.NATGatewayID,
		},
	}
	if err := m.ovn.AddNAT(ctx, router, snatRule); err != nil {
		return fmt.Errorf("add NAT GW snat %s -> %s on %s: %w", gw.SubnetCIDR, gw.PublicIP, router, err)
	}
	// Block until SB + chassis have the SNAT flow.
	if err := m.barrier(); err != nil {
		slog.Warn("policy: AddNATGateway flows barrier failed",
			"public_ip", gw.PublicIP, "subnet_cidr", gw.SubnetCIDR, "err", err)
	}
	return nil
}

func (m *natManager) DeleteNATGateway(ctx context.Context, vpcID, subnetCIDR string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "snat", subnetCIDR); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete NAT GW snat %s on %s: %w", subnetCIDR, router, err)
	}
	return nil
}

func (m *natManager) AddSNAT(ctx context.Context, vpcID, vpcCIDR, externalIP string) error {
	router := topology.VPCRouter(vpcID)
	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: externalIP,
		LogicalIP:  vpcCIDR,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcID,
			"spinifex:role":   "igw-default-snat",
		},
	}
	if err := m.ovn.AddNAT(ctx, router, snatRule); err != nil {
		return fmt.Errorf("add IGW snat %s -> %s on %s: %w", vpcCIDR, externalIP, router, err)
	}
	return nil
}

func (m *natManager) DeleteSNAT(ctx context.Context, vpcID, vpcCIDR string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "snat", vpcCIDR); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete IGW snat %s on %s: %w", vpcCIDR, router, err)
	}
	return nil
}

func (m *natManager) AddSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP, externalIP string) error {
	router := topology.VPCRouter(vpcID)
	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: externalIP,
		LogicalIP:  logicalIP,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":    vpcID,
			"spinifex:public_ip": externalIP,
			"spinifex:role":      "system-instance-egress",
		},
	}
	// Skip when the existing row already matches (idempotent re-publish guard).
	if existing, err := m.ovn.FindNATByExternalIP(ctx, "snat", externalIP); err != nil {
		slog.Warn("policy: AddSystemInstanceSNAT idempotency lookup failed", "external_ip", externalIP, "err", err)
	} else if existing != nil && existing.LogicalIP == logicalIP {
		slog.Info("policy: AddSystemInstanceSNAT idempotent skip — rule already current",
			"router", router, "external_ip", externalIP, "logical_ip", logicalIP)
		return nil
	}

	if err := m.ovn.AddNAT(ctx, router, snatRule); err != nil {
		return fmt.Errorf("add system-instance snat %s -> %s on %s: %w", logicalIP, externalIP, router, err)
	}
	// Block until SB + chassis have the SNAT flow.
	if err := m.barrier(); err != nil {
		slog.Warn("policy: AddSystemInstanceSNAT flows barrier failed",
			"logical_ip", logicalIP, "external_ip", externalIP, "err", err)
	}
	return nil
}

func (m *natManager) DeleteSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "snat", logicalIP); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete system-instance snat %s on %s: %w", logicalIP, router, err)
	}
	return nil
}
