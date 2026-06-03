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

	// DeleteEIP removes the rule by LogicalIP; idempotent.
	DeleteEIP(ctx context.Context, vpcID, logicalIP string) error

	// AddNATGateway installs the (snat, SubnetCIDR) rule; rejects overlap.
	AddNATGateway(ctx context.Context, gw NATGWSpec) error

	// DeleteNATGateway removes the (snat, SubnetCIDR) rule; idempotent.
	DeleteNATGateway(ctx context.Context, vpcID, subnetCIDR string) error

	// AddSNAT installs the IGW default-outbound snat rewriting VPC CIDR
	// to ExternalIP. AWS-parity callers typically skip this.
	AddSNAT(ctx context.Context, vpcID, vpcCIDR, externalIP string) error

	// DeleteSNAT removes the IGW default-outbound snat for vpcCIDR.
	DeleteSNAT(ctx context.Context, vpcID, vpcCIDR string) error

	// AddSystemInstanceSNAT installs an egress-only snat rewriting a single
	// system instance's /32 (logicalIP) to externalIP. Unlike AddEIP it is a
	// plain snat, not dnat_and_snat, so there is no inbound path to the
	// instance. Idempotent; tagged role=system-instance-egress.
	AddSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP, externalIP string) error

	// DeleteSystemInstanceSNAT removes the egress-only snat by logicalIP;
	// idempotent.
	DeleteSystemInstanceSNAT(ctx context.Context, vpcID, logicalIP string) error
}

// FlowsBarrier blocks until ovn-northd has compiled NB → SB and every
// chassis installed flows. Production wires a closure over
// `ovn-nbctl --wait=hv sync`; tests leave it nil.
type FlowsBarrier func() error

// GARPEmitter forces ovn-controller to emit a gratuitous ARP for the EIP so
// upstream switches and host ARP caches relearn the MAC owning that IP.
// Required after AddEIP when the external IP is being recycled onto a
// different LR — ovn-controller skips its automatic GARP when the LSP
// binding stays on the same chassis, leaving upstream ARP entries pointed
// at the prior chassis-redirect MAC until the kernel ARP timeout expires
// (60-300s).
//
// Best-effort: implementations return errors but callers warn and proceed.
type GARPEmitter func(EIPSpec) error

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

// WithGARPEmitter injects the post-AddEIP gratuitous-ARP hook. Without it,
// AddEIP relies on OVN's automatic GARP-on-binding-migration, which does not
// fire when the new LSP binds to the same chassis as the prior occupant.
func WithGARPEmitter(g GARPEmitter) Option {
	return func(m *natManager) {
		if g != nil {
			m.garp = g
		}
	}
}

type natManager struct {
	ovn     ovn.Client
	mode    NATMode
	barrier FlowsBarrier
	garp    GARPEmitter
}

var _ NATManager = (*natManager)(nil)

// NewNATManager constructs a NATManager bound to one NAT mode (must not be
// NATModeUnknown).
func NewNATManager(client ovn.Client, mode NATMode, opts ...Option) (NATManager, error) {
	if mode == NATModeUnknown {
		return nil, fmt.Errorf("NAT mode is unknown; resolve from host.Wiring.UplinkMode()")
	}
	m := &natManager{
		ovn:     client,
		mode:    mode,
		barrier: func() error { return nil },
		garp:    func(EIPSpec) error { return nil },
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
		slog.Info("policy: AddEIP idempotent skip — rule already current",
			"router", router, "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP)
		return nil
	}

	// Search every router for stale rules — vpc.delete-nat is
	// fire-and-forget and may not have run before IP reuse.
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
	if err := m.garp(eip); err != nil {
		slog.Warn("policy: AddEIP gratuitous-ARP emission failed", "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP, "err", err)
	}
	return nil
}

func (m *natManager) DeleteEIP(ctx context.Context, vpcID, logicalIP string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "dnat_and_snat", logicalIP); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete dnat_and_snat %s on %s: %w", logicalIP, router, err)
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
	// Block until SB + chassis have the SNAT flow; otherwise first packets
	// from the private subnet drop.
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
	// Skip when the existing row already matches; avoids the duplicate-append
	// on a re-published add (the snat is keyed by its unique pool external IP).
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
	// Block until SB + chassis have the SNAT flow; otherwise first egress
	// packets from the instance drop.
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
