// Package reconcile is the network stack's intent-actual reconciliation layer.
// It loads desired state from JetStream KV and applies the diff against OVN NB
// in topological order (VPC→Subnet→SG→Port→IGW→EIP→NATGW). Drift every 5m.
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

// GatewayClaimVerifier confirms ovn-controller has claimed the SB chassisredirect
// Port_Binding and nudges recompute if not. After a reboot the SB binding can be
// unclaimed while NB intent is correct, making VPC floating IPs unreachable.
type GatewayClaimVerifier interface {
	// GatewayPortClaimed reports whether the SB Port_Binding for crPortName (the
	// chassisredirect port) has a non-empty chassis.
	GatewayPortClaimed(ctx context.Context, crPortName string) (bool, error)
	// NudgeRecompute asks the local ovn-controller to re-evaluate logical flows.
	NudgeRecompute(ctx context.Context) error
	// GatewayReachable reports whether the external datapath actually forwards to
	// gwIP (the gateway LRP IP). A claimed SB binding does not prove flows are
	// installed: post-reboot ovn-controller can claim the chassisredirect port yet
	// leave stale gateway/localnet flows, so every control-plane signal is green
	// while EIPs stay unreachable. Used as a fallback for VPCs with no EIP; the LRP
	// IP OVN answers natively, so it cannot detect a stranded EIP NAT pipeline.
	GatewayReachable(ctx context.Context, gwIP string) (bool, error)
	// EIPReachable reports whether the NAT external-IP datapath for eip is live, by
	// forcing a fresh ARP resolution of the EIP. OVN answers ARP for a dnat_and_snat
	// external IP from the gateway chassis independent of the guest behind it, so a
	// resolved MAC proves the WAN uplink forwards and the EIP NAT flows are installed
	// — the signal the gateway LRP IP cannot give. Preferred over GatewayReachable.
	EIPReachable(ctx context.Context, eip string) (bool, error)
	// RepairDatapath re-asserts the host external uplink, then forces a recompute.
	// The post-reboot boot race that strands the datapath has two shapes: the veth
	// gluing br-ext to the WAN bridge comes up admin-down (a recompute cannot revive
	// a dead link), or its OVS ofport renumbered and the gateway flows went stale (a
	// recompute fixes that). Bringing the veth up (a no-op in physical mode where the
	// device is absent) then recomputing covers both, idempotently.
	RepairDatapath(ctx context.Context) error
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
	// IMDS installs IMDS OVN topology per-VPC. Optional: nil skips IMDS (tests).
	IMDS    external.IMDSTopologyManager
	LocalAZ string
	// NodeHostname is the holder identity for leader-election CAS.
	NodeHostname string
	// Chassis is the SBDB-discovered chassis list for gateway LRP rebinding.
	Chassis []string
	// GatewayClaim verifies/repairs the SB chassis claim after rebinding. Optional.
	GatewayClaim GatewayClaimVerifier
	// DNSServer is the OVN dhcp_options dns_server value ("{a, b}"). Empty falls
	// back to the topology default to keep both code paths in sync.
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

// Reconcile diffs intent vs. actual OVN state and applies in topological order.
// Per-stage errors are logged; only a scan failure is returned.
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
	r.applyPorts(ctx, intent, actual, pruneOrphans)
	r.applyIGWs(ctx, intent, actual)
	r.applyEIPs(ctx, intent, actual)
	r.applyNATGWs(ctx, intent, actual)
	r.applyIGWRoutes(ctx, intent, actual)
	r.applyNATGWRoutes(ctx, intent, actual)
	r.applyDropGates(ctx, intent, actual)

	slog.Info("reconcile: complete")
	return nil
}
