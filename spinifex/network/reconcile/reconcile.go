// Package reconcile is the network stack's intent-actual reconciliation
// layer. It loads desired state from the JetStream KV snapshot (scoped to
// the local AZ) and applies the diff against OVN NB in a single
// topologically-sorted pass (VPC → Subnet → SG → Port → IGW → EIP →
// NATGW). Deletes run reverse order. Drift fires every 5 minutes.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// Reconciler converges OVN NB DB to a declared IntentState. Implementations
// are idempotent: a second call with the same IntentState is a no-op.
type Reconciler interface {
	Reconcile(ctx context.Context, intent IntentState) error
	// ReconcileApplyOnly skips orphan-pruning. Startup uses this to avoid
	// racing peer subscribers that haven't processed in-flight create events
	// yet; legitimate orphans are pruned on the next drift tick.
	ReconcileApplyOnly(ctx context.Context, intent IntentState) error
}

// GatewayClaimVerifier confirms that ovn-controller has claimed the Southbound
// Port_Binding for a gateway router port, and nudges a recompute when it has
// not. After a host reboot the Northbound gateway_chassis intent can be correct
// while the SB binding is left unclaimed — ovn-controller then installs no
// logical flows for the centralised gateway redirect and the VPC's floating IPs
// go dark. The reconciler runs this after SetGatewayChassis to drive the binding
// to claimed. Implementations shell out to ovn-sbctl/ovn-appctl; the
// compile-time check lives at the wiring site (vpcd) since the host
// implementation cannot import this cross-cutter package.
type GatewayClaimVerifier interface {
	// GatewayPortClaimed reports whether the SB Port_Binding for lrpName has a
	// non-empty chassis.
	GatewayPortClaimed(ctx context.Context, lrpName string) (bool, error)
	// NudgeRecompute asks the local ovn-controller to re-evaluate logical flows.
	NudgeRecompute(ctx context.Context) error
}

// Config is the construction-time bag for the reconciler. All fields except
// Chassis are required.
type Config struct {
	OVN      ovn.Client
	SG       policy.SecurityGroupManager
	NAT      policy.NATManager
	Routes   policy.RouteManager
	IGW      external.IGWManager
	Topology topology.Manager
	// IMDS installs per-VPC IMDS OVN topology during the VPC apply pass.
	// Optional: nil skips IMDS plumbing (focused reconcile tests leave it
	// unset; production always wires it).
	IMDS    external.IMDSTopologyManager
	LocalAZ string
	// NodeHostname is the holder identity for leader-election CAS.
	NodeHostname string
	// Chassis is the SBDB-discovered chassis list for gateway LRP rebinding.
	Chassis []string
	// GatewayClaim verifies/repairs the Southbound chassis claim for gateway
	// ports after rebinding. Optional: nil skips the post-reboot claim check
	// (focused reconcile tests leave it unset; production wires the host prober).
	GatewayClaim GatewayClaimVerifier
	// DNSServer is the OVN dhcp_options dns_server value ("{a, b}") emitted on
	// subnet DHCPOptions rows. Empty falls back to the topology default, so the
	// reconciler and live topology paths emit the same value (no drift).
	DNSServer string
}

type reconciler struct {
	ovn       ovn.Client
	sg        policy.SecurityGroupManager
	nat       policy.NATManager
	routes    policy.RouteManager
	igw       external.IGWManager
	topology  topology.Manager
	imds      external.IMDSTopologyManager
	localAZ   string
	host      string
	chassis   []string
	gwClaim   GatewayClaimVerifier
	dnsServer string
}

var _ Reconciler = (*reconciler)(nil)

// New constructs a Reconciler from cfg. Returns an error when any required
// field is missing.
func New(cfg Config) (Reconciler, error) {
	switch {
	case cfg.OVN == nil:
		return nil, errors.New("reconcile: OVN client required")
	case cfg.SG == nil:
		return nil, errors.New("reconcile: SecurityGroupManager required")
	case cfg.NAT == nil:
		return nil, errors.New("reconcile: NATManager required")
	case cfg.Routes == nil:
		return nil, errors.New("reconcile: RouteManager required")
	case cfg.IGW == nil:
		return nil, errors.New("reconcile: IGWManager required")
	case cfg.Topology == nil:
		return nil, errors.New("reconcile: Topology manager required")
	case cfg.NodeHostname == "":
		return nil, errors.New("reconcile: NodeHostname required")
	}
	dnsServer := cfg.DNSServer
	if dnsServer == "" {
		dnsServer = topology.FormatDNSServerList(nil)
	}
	return &reconciler{
		ovn:       cfg.OVN,
		sg:        cfg.SG,
		nat:       cfg.NAT,
		routes:    cfg.Routes,
		igw:       cfg.IGW,
		topology:  cfg.Topology,
		imds:      cfg.IMDS,
		localAZ:   cfg.LocalAZ,
		host:      cfg.NodeHostname,
		chassis:   cfg.Chassis,
		gwClaim:   cfg.GatewayClaim,
		dnsServer: dnsServer,
	}, nil
}

// Reconcile diffs intent vs. actual OVN state and applies create/delete
// per-resource: VPC → Subnet → SG → Port → IGW → EIP → NATGW (reverse for
// deletes). SG precedes Port so ENI LSPs join their PGs atomically via
// CreateLogicalSwitchPortInGroups. Per-stage errors are logged and the next
// stage runs; only an actual-state scan failure is returned.
func (r *reconciler) Reconcile(ctx context.Context, intent IntentState) error {
	return r.reconcile(ctx, intent, true)
}

// ReconcileApplyOnly is documented on the Reconciler interface.
func (r *reconciler) ReconcileApplyOnly(ctx context.Context, intent IntentState) error {
	return r.reconcile(ctx, intent, false)
}

func (r *reconciler) reconcile(ctx context.Context, intent IntentState, pruneOrphans bool) error {
	actual, err := scanActual(ctx, r.ovn)
	if err != nil {
		return fmt.Errorf("scan actual OVN state: %w", err)
	}

	slog.Info("reconcile: starting",
		"local_az", r.localAZ,
		"prune_orphans", pruneOrphans,
		"intent_vpcs", len(intent.VPCs),
		"intent_subnets", len(intent.Subnets),
		"intent_ports", len(intent.Ports),
		"intent_sgs", len(intent.SGs),
		"intent_igws", len(intent.IGWs),
		"intent_eips", len(intent.EIPs),
		"intent_natgws", len(intent.NATGWs),
		"intent_igw_routes", len(intent.IGWRoutes),
		"intent_natgw_routes", len(intent.NATGWRoutes),
	)

	r.applyVPCs(ctx, intent, actual)
	r.applySubnets(ctx, intent, actual)
	r.applySGs(ctx, intent, actual, pruneOrphans)
	r.applyPorts(ctx, intent, actual)
	r.applyIGWs(ctx, intent, actual)
	r.applyEIPs(ctx, intent, actual)
	r.applyNATGWs(ctx, intent, actual)
	r.applyIGWRoutes(ctx, intent, actual)
	r.applyNATGWRoutes(ctx, intent, actual)
	r.applyDropGates(ctx, intent, actual)

	slog.Info("reconcile: complete")
	return nil
}
