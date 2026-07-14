package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// GatewayIPExtIDKey is the gateway LRP external_ids key carrying the IP
// allocated at IGW attach; the routed-mode next hop for host EIP routes.
const GatewayIPExtIDKey = "spinifex:gateway_ip"

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

	// DeleteEIP removes the (ExternalIP, LogicalIP) rule and flushes the host
	// ARP entry for ExternalIP so a recycled IP is not shadowed by the stale
	// MAC. portName, when set, owner-scopes the delete: a row whose stamped
	// logical port differs has been reassigned to a live ENI, so the stale
	// delete is a no-op even when the (ExternalIP, LogicalIP) pair recycles
	// identically. Idempotent.
	DeleteEIP(ctx context.Context, vpcID, externalIP, logicalIP, portName string) error

	// PruneOrphanEIPs deletes every dnat_and_snat row whose stamped
	// spinifex:logical_port owning ENI is absent from livePorts (the set of live
	// intent port names), flushing the host ARP entry and unbinding routed-mode
	// host state for each. Rows with no stamped logical port are left untouched
	// (owner undeterminable). Returns the number of rows removed.
	PruneOrphanEIPs(ctx context.Context, livePorts map[string]struct{}) (int, error)

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

// HostEIPBinder plumbs routed-mode host state for an EIP: the /32 route into
// OVN via the gateway LRP and proxy-ARP on the uplink. Bind fires on every
// AddEIP (fresh and idempotent) so reconcile re-ensures host state after
// reboot; Unbind fires on DeleteEIP.
type HostEIPBinder struct {
	Bind   func(eip EIPSpec, gwLrpIP string) error
	Unbind func(externalIP string) error
}

// WithHostEIPBinder injects the routed-mode host plumbing hooks fired on EIP
// attach/detach. Only consulted in NATModeRouted.
func WithHostEIPBinder(b HostEIPBinder) Option {
	return func(m *natManager) {
		if b.Bind != nil && b.Unbind != nil {
			m.hostBinder = &b
		}
	}
}

// NATExemptSetName is the singleton Address_Set holding destinations that
// skip routed-mode NAT (transit /24 plus operator extras).
const NATExemptSetName = "spinifex_nat_exempt"

// WithSNATExemptSet names an Address_Set whose CIDRs skip NAT (stamped as
// exempted_ext_ips on SNAT/EIP rows). Routed mode uses it so VM replies to
// host-initiated flows keep their private source. The set is never deleted:
// it is a singleton strongly referenced by every routed NAT row.
func WithSNATExemptSet(setName string, cidrs []string) Option {
	return func(m *natManager) {
		if setName != "" {
			m.exemptSetName = setName
			m.exemptCIDRs = cidrs
		}
	}
}

type natManager struct {
	ovn           ovn.Client
	mode          NATMode
	barrier       FlowsBarrier
	neigh         NeighFlusher
	neighPrime    NeighPrimer
	exemptSetName string
	exemptCIDRs   []string
	hostBinder    *HostEIPBinder
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

// exemptSetUUID ensures the configured exempt Address_Set exists and returns
// its UUID for stamping as exempted_ext_ips. Nil when the option is unset or
// the mode is not routed (only routed SNAT breaks host-initiated return paths).
func (m *natManager) exemptSetUUID(ctx context.Context) (*string, error) {
	if m.mode != NATModeRouted || m.exemptSetName == "" {
		return nil, nil
	}
	uuid, err := m.ovn.EnsureAddressSet(ctx, m.exemptSetName, m.exemptCIDRs)
	if err != nil {
		return nil, fmt.Errorf("ensure NAT exempt set %q: %w", m.exemptSetName, err)
	}
	return &uuid, nil
}

func (m *natManager) AddEIP(ctx context.Context, eip EIPSpec) error {
	router := topology.VPCRouter(eip.VPCID)

	exemptUUID, err := m.exemptSetUUID(ctx)
	if err != nil {
		return err
	}

	natRule := &nbdb.NAT{
		Type:       "dnat_and_snat",
		ExternalIP: eip.ExternalIP,
		LogicalIP:  eip.LogicalIP,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":    eip.VPCID,
			"spinifex:public_ip": eip.ExternalIP,
		},
	}
	// Stamp the owning ENI port so DeleteEIP can owner-scope a stale delete.
	// Centralised mode leaves the native LogicalPort unset, so the ExternalID
	// is the portable discriminator across both NAT modes.
	if eip.PortName != "" {
		natRule.ExternalIDs["spinifex:logical_port"] = eip.PortName
	}
	natRule.ExemptedExtIps = exemptUUID
	distributed := m.mode == NATModeDistributed && eip.PortName != "" && eip.MAC != ""
	if distributed {
		mac := eip.MAC
		port := eip.PortName
		natRule.ExternalMAC = &mac
		natRule.LogicalPort = &port
	}

	// Look up the existing row to decide between an idempotent skip and a re-point.
	existing, lookupErr := m.ovn.FindNATByExternalIP(ctx, "dnat_and_snat", eip.ExternalIP)
	if lookupErr != nil {
		slog.Warn("policy: AddEIP idempotency lookup failed", "external_ip", eip.ExternalIP, "err", lookupErr)
	}
	// A changed owning ENI must force a re-point even when external+logical IP are
	// unchanged. The spinifex:logical_port external-id is stamped in both NAT modes
	// (the native LogicalPort column is empty in centralised mode), so it is the
	// portable discriminator when a recycled IP pair is taken over by a new ENI —
	// without it the datapath keeps targeting the dead predecessor's port.
	ownerChanged := existing != nil && eip.PortName != "" &&
		existing.ExternalIDs["spinifex:logical_port"] != eip.PortName

	// Skip when the existing row already matches; avoids the delete-then-add
	// flow-install gap on duplicate publishes. Never skip on an owner change.
	if existing != nil && !ownerChanged && existing.LogicalIP == eip.LogicalIP &&
		existing.ExternalIDs["spinifex:vpc_id"] == eip.VPCID &&
		(!distributed ||
			(existing.ExternalMAC != nil && *existing.ExternalMAC == eip.MAC &&
				existing.LogicalPort != nil && *existing.LogicalPort == eip.PortName)) {
		// Upgrade path: stamp the exempt ref in place on rows minted before the
		// option existed (or after a set re-create).
		if exemptUUID != nil && (existing.ExemptedExtIps == nil || *existing.ExemptedExtIps != *exemptUUID) {
			if err := m.ovn.SetNATExemptedExtIPs(ctx, router, "dnat_and_snat", eip.LogicalIP, exemptUUID); err != nil {
				return fmt.Errorf("patch exempt set on dnat_and_snat %s on %s: %w", eip.ExternalIP, router, err)
			}
			slog.Info("policy: AddEIP patched exempt set on existing rule",
				"router", router, "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP)
		}
		slog.Info("policy: AddEIP idempotent skip — rule current, re-priming reachability",
			"router", router, "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP)
		// Skip row churn but still re-prime: stop->start re-attaches the same EIP
		// and the host neigh stays dark until ARP times out without a fresh prime.
		m.primeReachability(ctx, eip, distributed)
		// Host state (routes, proxy-ARP) is volatile even when the OVN row
		// survives — reconcile lands here after a reboot, so re-bind.
		return m.bindHostEIP(ctx, eip)
	}
	if ownerChanged {
		slog.Info("policy: AddEIP re-pointing dnat_and_snat — owning ENI changed",
			"router", router, "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP,
			"old_port", existing.ExternalIDs["spinifex:logical_port"], "new_port", eip.PortName)
	}

	// Search every router for stale rules — vpc.delete-nat is fire-and-forget.
	if removed, err := m.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", eip.ExternalIP); err != nil {
		slog.Warn("policy: stale NAT cleanup failed before AddEIP", "external_ip", eip.ExternalIP, "err", err)
	} else if removed > 0 {
		slog.Info("policy: cleaned stale dnat_and_snat rules before AddEIP", "external_ip", eip.ExternalIP, "removed", removed)
	}

	// Scrub any predecessor row sharing this private IP under a different external
	// IP. A recycled private IP that lands on a new EIP leaves the terminated
	// owner's dnat_and_snat (old EIP -> this private IP) intact; its SNAT half is
	// keyed on logical_ip and wins the guest's return path, rewriting replies to
	// the old EIP and blackholing the new one. dnat_and_snat is 1:1 per private IP,
	// so any row on this router for this logical IP is stale here. Router-scoped:
	// private IPs repeat across VPCs.
	if err := m.ovn.DeleteNAT(ctx, router, "dnat_and_snat", eip.LogicalIP); err != nil &&
		!errors.Is(err, ovn.ErrNATNotFound) {
		slog.Warn("policy: stale logical-IP NAT cleanup failed before AddEIP",
			"logical_ip", eip.LogicalIP, "err", err)
	}

	if err := m.ovn.AddNAT(ctx, router, natRule); err != nil {
		return fmt.Errorf("add dnat_and_snat %s -> %s on %s: %w", eip.LogicalIP, eip.ExternalIP, router, err)
	}
	if err := m.barrier(); err != nil {
		slog.Warn("policy: AddEIP flows barrier failed", "external_ip", eip.ExternalIP, "logical_ip", eip.LogicalIP, "err", err)
	}
	m.primeReachability(ctx, eip, distributed)
	return m.bindHostEIP(ctx, eip)
}

// bindHostEIP fires the routed-mode host plumbing hook for an EIP. No-op in
// other modes or when no binder is configured. Errors are returned so a
// half-plumbed EIP surfaces to the caller (reconcile retries the bind).
func (m *natManager) bindHostEIP(ctx context.Context, eip EIPSpec) error {
	if m.mode != NATModeRouted || m.hostBinder == nil {
		return nil
	}
	gwLrpIP := m.gatewayPortIP(ctx, eip.VPCID)
	if gwLrpIP == "" {
		return fmt.Errorf("bind host EIP %s: gateway LRP IP unknown for %s (IGW attached?)", eip.ExternalIP, eip.VPCID)
	}
	if err := m.hostBinder.Bind(eip, gwLrpIP); err != nil {
		return fmt.Errorf("bind host EIP %s via %s: %w", eip.ExternalIP, gwLrpIP, err)
	}
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

// gatewayPortIP returns the IP of the VPC gateway router's external port — the
// transit-side next hop for routed-mode host EIP routes. Prefers the IP stamped
// at IGW attach; falls back to the LRP network address. Empty on lookup miss.
func (m *natManager) gatewayPortIP(ctx context.Context, vpcID string) string {
	lrp, err := m.ovn.GetLogicalRouterPort(ctx, topology.GatewayRouterPort(vpcID))
	if err != nil || lrp == nil {
		return ""
	}
	if ip := lrp.ExternalIDs[GatewayIPExtIDKey]; ip != "" {
		return ip
	}
	if len(lrp.Networks) > 0 {
		if pfx, err := netip.ParsePrefix(lrp.Networks[0]); err == nil {
			return pfx.Addr().String()
		}
	}
	return ""
}

func (m *natManager) DeleteEIP(ctx context.Context, vpcID, externalIP, logicalIP, portName string) error {
	router := topology.VPCRouter(vpcID)
	// An empty external IP means there is no EIP and therefore no dnat_and_snat
	// row to remove.
	if externalIP == "" {
		return nil
	}
	// Scope the delete to the (external_ip, logical_ip) pair and, when known, the
	// owning ENI port. External IPs are recycled from the pool as instances come
	// and go, and vpc.delete-nat is fire-and-forget plus re-emitted by the GC
	// teardown sweep, so a stale or duplicated delete can arrive after the IP —
	// and even the identical private IP — has been reassigned to a live instance.
	// Deleting on a pair that recycled identically would tear down the new owner's
	// rule and ARP entry; the stamped logical port is the discriminator that
	// survives identical-pair reuse.
	if logicalIP != "" || portName != "" {
		existing, err := m.ovn.FindNATByExternalIP(ctx, "dnat_and_snat", externalIP)
		switch {
		case err != nil:
			slog.Warn("policy: DeleteEIP ownership lookup failed, proceeding with delete",
				"external_ip", externalIP, "logical_ip", logicalIP, "err", err)
		case existing == nil:
			return nil
		case logicalIP != "" && existing.LogicalIP != logicalIP:
			slog.Info("policy: DeleteEIP skip — external IP reassigned to a different logical IP (stale delete)",
				"external_ip", externalIP, "stale_logical_ip", logicalIP, "current_logical_ip", existing.LogicalIP)
			return nil
		case portName != "" && existing.ExternalIDs["spinifex:logical_port"] != "" &&
			existing.ExternalIDs["spinifex:logical_port"] != portName:
			slog.Info("policy: DeleteEIP skip — external IP reassigned to a different ENI (stale delete)",
				"external_ip", externalIP, "stale_port", portName,
				"current_port", existing.ExternalIDs["spinifex:logical_port"])
			return nil
		}
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
	// Tear down routed-mode host plumbing. Best-effort: the row is already gone
	// and stale host state is re-converged by the next bind of the same IP.
	if m.mode == NATModeRouted && m.hostBinder != nil {
		if err := m.hostBinder.Unbind(externalIP); err != nil {
			slog.Warn("policy: DeleteEIP host unbind failed", "external_ip", externalIP, "err", err)
		}
	}
	return nil
}

func (m *natManager) PruneOrphanEIPs(ctx context.Context, livePorts map[string]struct{}) (int, error) {
	nats, err := m.ovn.ListNATs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list NATs for orphan EIP prune: %w", err)
	}
	pruned := 0
	for i := range nats {
		n := nats[i]
		if n.Type != "dnat_and_snat" {
			continue
		}
		port := n.ExternalIDs["spinifex:logical_port"]
		// No stamp: owner undeterminable (legacy row). Leave it so a live EIP
		// whose owner cannot be proven absent is never swept.
		if port == "" {
			continue
		}
		if _, live := livePorts[port]; live {
			continue
		}
		removed, derr := m.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", n.ExternalIP)
		if derr != nil {
			slog.Warn("policy: orphan EIP prune delete failed",
				"external_ip", n.ExternalIP, "logical_port", port, "err", derr)
			continue
		}
		if removed == 0 {
			continue
		}
		pruned += removed
		// Flush host ARP so the freed external IP is not shadowed by the dead
		// owner's MAC, and tear down routed-mode host plumbing. Best-effort.
		if err := m.neigh(n.ExternalIP); err != nil {
			slog.Warn("policy: orphan EIP prune neighbour flush failed", "external_ip", n.ExternalIP, "err", err)
		}
		if m.mode == NATModeRouted && m.hostBinder != nil {
			if err := m.hostBinder.Unbind(n.ExternalIP); err != nil {
				slog.Warn("policy: orphan EIP prune host unbind failed", "external_ip", n.ExternalIP, "err", err)
			}
		}
		slog.Info("policy: pruned orphan dnat_and_snat — owning ENI absent from intent",
			"external_ip", n.ExternalIP, "logical_ip", n.LogicalIP, "logical_port", port)
	}
	return pruned, nil
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
	// Reconcile the existing row, keyed on (router, subnet CIDR) — DeleteNAT's key —
	// so the reconcile's re-publish is a no-op and a multi-subnet NAT GW dedups per
	// subnet instead of minting duplicate snat rows that survive teardown.
	if existing, err := m.ovn.FindNATByLogicalIP(ctx, router, "snat", gw.SubnetCIDR); err != nil {
		slog.Warn("policy: AddNATGateway idempotency lookup failed", "subnet_cidr", gw.SubnetCIDR, "err", err)
	} else if existing != nil && existing.ExternalIP == gw.PublicIP {
		slog.Info("policy: AddNATGateway idempotent skip — rule already current",
			"router", router, "public_ip", gw.PublicIP, "subnet_cidr", gw.SubnetCIDR)
		return nil
	} else if existing != nil {
		// Same subnet CIDR, different public IP (e.g. a dropped delete then a recreate
		// with a new EIP). Scrub the stale row(s) so the new EIP does not leak egress
		// via the old one; delete-all also clears any accumulated duplicates.
		slog.Info("policy: AddNATGateway replacing stale snat — public IP changed",
			"router", router, "old_ip", existing.ExternalIP, "new_ip", gw.PublicIP, "subnet_cidr", gw.SubnetCIDR)
		if err := m.ovn.DeleteNAT(ctx, router, "snat", gw.SubnetCIDR); err != nil && !errors.Is(err, ovn.ErrNATNotFound) {
			return fmt.Errorf("replace stale NAT GW snat %s on %s: %w", gw.SubnetCIDR, router, err)
		}
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

	exemptUUID, err := m.exemptSetUUID(ctx)
	if err != nil {
		return err
	}

	// Reconcile replays IGW attach every pass; key on (router, vpcCIDR) so the
	// re-publish is a no-op instead of minting duplicate snat rows.
	if existing, err := m.ovn.FindNATByLogicalIP(ctx, router, "snat", vpcCIDR); err != nil {
		slog.Warn("policy: AddSNAT idempotency lookup failed", "vpc_cidr", vpcCIDR, "err", err)
	} else if existing != nil && existing.ExternalIP == externalIP {
		// Upgrade path: stamp the exempt ref in place on rows minted before the
		// option existed (or after a set re-create).
		if exemptUUID != nil && (existing.ExemptedExtIps == nil || *existing.ExemptedExtIps != *exemptUUID) {
			if err := m.ovn.SetNATExemptedExtIPs(ctx, router, "snat", vpcCIDR, exemptUUID); err != nil {
				return fmt.Errorf("patch exempt set on IGW snat %s on %s: %w", vpcCIDR, router, err)
			}
			slog.Info("policy: AddSNAT patched exempt set on existing rule",
				"router", router, "vpc_cidr", vpcCIDR, "external_ip", externalIP)
			return nil
		}
		slog.Info("policy: AddSNAT idempotent skip — rule already current",
			"router", router, "vpc_cidr", vpcCIDR, "external_ip", externalIP)
		return nil
	} else if existing != nil {
		// Same VPC CIDR, different external IP (gateway transit IP changed).
		// Scrub the stale row so egress does not leak via the old IP.
		slog.Info("policy: AddSNAT replacing stale snat — external IP changed",
			"router", router, "old_ip", existing.ExternalIP, "new_ip", externalIP, "vpc_cidr", vpcCIDR)
		if err := m.ovn.DeleteNAT(ctx, router, "snat", vpcCIDR); err != nil && !errors.Is(err, ovn.ErrNATNotFound) {
			return fmt.Errorf("replace stale IGW snat %s on %s: %w", vpcCIDR, router, err)
		}
	}

	snatRule := &nbdb.NAT{
		Type:           "snat",
		ExternalIP:     externalIP,
		LogicalIP:      vpcCIDR,
		ExemptedExtIps: exemptUUID,
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
