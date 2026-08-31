// Package reconcile is the network stack's intent-actual reconciliation layer.
// It loads desired state from JetStream KV and applies the diff against OVN NB
// in topological order (VPC→Subnet→SG→Port→IGW→EIP→NATGW). Drift every 5m.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// ErrPassIncomplete marks a pass that applied what it could but left at least
// one resource unconverged. A pass keeps applying past a per-resource failure,
// so without this the caller cannot tell a clean pass from one that dropped ten
// resources on the floor. DriftLoop tests for it with errors.Is and requeues on
// a short backoff instead of waiting a full DriftInterval.
var ErrPassIncomplete = errors.New("reconcile: pass incomplete")

// Reconciler converges OVN NB DB to a declared IntentState. Implementations
// are idempotent: a second call with the same IntentState is a no-op.
type Reconciler interface {
	// Reconcile returns a scan failure as-is, and wraps ErrPassIncomplete when
	// the scan succeeded but some resources did not converge.
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
	// GuestPortUp reports whether the SB Port_Binding for a guest ENI LSP is up
	// (bound to a chassis with flows installed). A guest port that is not up means
	// the gatewayLRP->guest flow is not installed, so the ingress EIP datapath
	// blackholes after DNAT and the public IP is dark against an otherwise-running
	// instance — the post-reboot state until an ovn-controller recompute binds it.
	GuestPortUp(ctx context.Context, lspName string) (bool, error)
	// SBConnectionState returns ovn-controller's Southbound OVSDB connection status
	// ("connected" when healthy). A sustained non-"connected" status is the stale-SB
	// RAFT wedge that stops new Port_Binding realisation; a recompute cannot clear it
	// because it re-evaluates flows from the same stale SB view.
	SBConnectionState(ctx context.Context) (string, error)
	// ResetSBClusterState forces ovn-controller to re-sync its SB cluster state,
	// clearing a stale-index wedge without a process restart or a flow wipe. Escalated
	// to when a recompute nudge cannot converge a binding and the SB is not connected.
	ResetSBClusterState(ctx context.Context) error
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
	LocalAZ  string
	// NodeHostname is the holder identity for leader-election CAS.
	NodeHostname string
	// Chassis is the SBDB-discovered chassis list for gateway LRP rebinding.
	Chassis []string
	// GatewayClaim verifies/repairs the SB chassis claim after rebinding. Optional.
	GatewayClaim GatewayClaimVerifier
	// DNSServer is the OVN dhcp_options dns_server value (a single IP or
	// "{a, b}"). Empty falls back to the topology default to keep both code
	// paths in sync.
	DNSServer string
	// IPSecDisabled mirrors network.ipsec_enabled = false. It widens the MTU
	// advertised over DHCP, since a plaintext overlay has no 34-byte ESP header
	// to budget for. Inverted so the zero value is the conservative MTU: a
	// caller that leaves it unset advertises small, which is merely slower,
	// where advertising large on an encrypted path blackholes big segments.
	IPSecDisabled bool
	// UnderlayMTU is the fabric MTU the advertised guest MTU is derived from.
	// Zero falls back to the 1500 default, same as an unset config key.
	UnderlayMTU int
	// FreshIntent re-reads intent from the control-plane store on demand. Every
	// orphan sweep — NAT rows, SG port groups, guest LSPs — lists OVN live but is
	// otherwise driven by the intent snapshot captured at the start of the pass,
	// and the apply phase can block for tens of seconds. A resource created
	// mid-pass is therefore live but absent from that snapshot, and matching
	// against the snapshot alone sweeps it: a guest's public IP blackholes, or an
	// SG port group vanishes and every later ENI create in the VPC fails.
	// Optional: nil leaves the start-of-pass snapshot as the sole liveness source
	// (unit tests, or callers with no store).
	FreshIntent func(ctx context.Context) (IntentState, error)
	// MarkIGWAttached reports a confirmed IGW attachment back to the control
	// plane, so DescribeInternetGateways stops claiming an attachment exists
	// before one does. Called with the record key carried on the IGW spec.
	// Optional: nil leaves the record untouched.
	MarkIGWAttached func(ctx context.Context, recordKey string) error
}

type reconciler struct {
	ovn          ovn.Client
	sg           policy.SecurityGroupManager
	nat          policy.NATManager
	routes       policy.RouteManager
	igw          external.IGWManager
	topology     topology.Manager
	localAZ      string
	host         string
	chassis      []string
	gwClaim      GatewayClaimVerifier
	dnsServer    string
	ipsecEnabled bool
	underlayMTU  int
	reloadIntent func(ctx context.Context) (IntentState, error)
	markAttached func(ctx context.Context, recordKey string) error

	// Guest ports that burned their convergence deadline, so a port whose guest
	// is gone stops paying the full nudge sequence every cycle.
	portBackoffMu sync.Mutex
	portBackoff   map[string]portBackoffState
}

// portBackoffState is one guest port's consecutive failure count and the instant
// its backoff expires.
type portBackoffState struct {
	failures int
	until    time.Time
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
		ovn:          cfg.OVN,
		sg:           cfg.SG,
		nat:          cfg.NAT,
		routes:       cfg.Routes,
		igw:          cfg.IGW,
		topology:     cfg.Topology,
		localAZ:      cfg.LocalAZ,
		host:         cfg.NodeHostname,
		chassis:      cfg.Chassis,
		gwClaim:      cfg.GatewayClaim,
		dnsServer:    dnsServer,
		ipsecEnabled: !cfg.IPSecDisabled,
		underlayMTU:  cfg.UnderlayMTU,
		reloadIntent: cfg.FreshIntent,
		markAttached: cfg.MarkIGWAttached,
	}, nil
}

// passFailure is one resource an apply stage could not converge.
type passFailure struct {
	class string
	id    string
	err   error
}

// passResult accumulates per-resource apply failures so a pass that logged and
// continued past them still reports itself as incomplete to the drift loop.
type passResult struct {
	failures []passFailure
}

// fail records that id in class did not converge.
func (p *passResult) fail(class, id string, err error) {
	p.failures = append(p.failures, passFailure{class: class, id: id, err: err})
}

// summaryKV renders per-class failure counts as slog key/values, class-sorted so
// the summary line is stable across passes.
func (p *passResult) summaryKV() []any {
	counts := make(map[string]int, len(p.failures))
	for _, f := range p.failures {
		counts[f.class]++
	}
	kv := make([]any, 0, 2+2*len(counts))
	kv = append(kv, "unconverged", len(p.failures))
	for _, class := range slices.Sorted(maps.Keys(counts)) {
		kv = append(kv, class, counts[class])
	}
	return kv
}

// Failure classes for the per-pass convergence summary, one per apply stage so
// the summary names the stage that did not converge.
const (
	classVPC          = "vpc"
	classSubnet       = "subnet"
	classSG           = "sg"
	classPort         = "port"
	classIGW          = "igw"
	classEIP          = "eip"
	classNATGW        = "natgw"
	classIGWRoute     = "igw_route"
	classNATGWRoute   = "natgw_route"
	classDropGate     = "drop_gate"
	classPublicEgress = "public_egress"
)

// Reconcile diffs intent vs. actual OVN state and applies in topological order.
// Per-stage errors are logged and the pass continues; a scan failure is returned
// as-is and an unconverged resource surfaces as ErrPassIncomplete.
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

	res := &passResult{}
	r.applyVPCs(ctx, intent, actual, res)
	r.applySubnets(ctx, intent, actual, res)
	r.applySGs(ctx, intent, actual, pruneOrphans, res)
	r.applyPorts(ctx, intent, actual, pruneOrphans, res)
	r.applyIGWs(ctx, intent, actual, res)
	r.applyEIPs(ctx, intent, actual, res)
	if pruneOrphans {
		r.pruneOrphanEIPs(ctx, intent, res)
	}
	r.applyNATGWs(ctx, intent, actual, res)
	r.applyIGWRoutes(ctx, intent, actual, res)
	r.applyNATGWRoutes(ctx, intent, actual, res)
	r.applyDropGates(ctx, intent, actual, res)
	r.applyPublicInstanceEgress(ctx, intent, actual, res)

	if len(res.failures) == 0 {
		slog.Info("reconcile: complete", "converged", true)
		return nil
	}
	slog.Warn("reconcile: pass incomplete", res.summaryKV()...)
	return fmt.Errorf("reconcile: %d resource(s) unconverged: %w", len(res.failures), ErrPassIncomplete)
}
