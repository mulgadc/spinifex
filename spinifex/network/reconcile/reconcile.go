// Package reconcile is the spinifex network stack's intent-actual
// reconciliation layer. It reads the canonical desired state from the NATS
// JetStream KV snapshot, scopes it to the local availability zone, and
// applies the diff against OVN NB DB in a single topologically-sorted pass
// (VPC → Subnet → Port → SG ACLs → IGW → EIP/NATGW). Deletes run in reverse
// order.
//
// The reconciler replaces the legacy five-pass startup sequence
// (`Reconcile` + `ReconcileFromKV` + `ReconcileSGsOnce` +
// `RetrofitAllExternalLocalnetOptions` + `RetrofitAllGatewayPortNetworks`)
// in `services/vpcd`. A 5-minute drift goroutine drives the same call
// periodically, replacing the 30-second `ReconcileSGsLoop`.
//
// Multi-AZ scoping: `IntentState` is built by `LoadIntentFromKV(ctx, js,
// localAZ)`. The filter rule is `vpc.AZ == "" || vpc.AZ == localAZ` —
// records without an `AZ` field are legacy (pre-Phase 2.2) and are treated
// as local. New VPCs are stamped with the local AZ at create time by the
// `handlers/ec2/vpc.CreateVpc` path.
//
// See docs/development/feature/spinifex-network-redesign.md §11 and
// docs/development/feature/spinifex-network-redesign-phase2.md §2.2 for the
// authoritative contract.
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
}

// Config is the construction-time bag for the reconciler. Every field
// except Chassis is required.
type Config struct {
	OVN      ovn.Client
	SG       policy.SecurityGroupManager
	NAT      policy.NATManager
	Routes   policy.RouteManager
	IGW      external.IGWManager
	Topology topology.Manager
	LocalAZ  string
	// NodeHostname is the holder identity stamped on the leader-election
	// CAS row. Set to os.Hostname() in production.
	NodeHostname string
	// Chassis is the SBDB-discovered chassis list, used for gateway LRP
	// chassis rebinding during drift. Same value passed to TopologyHandler
	// in the legacy boot path.
	Chassis []string
}

type reconciler struct {
	ovn      ovn.Client
	sg       policy.SecurityGroupManager
	nat      policy.NATManager
	routes   policy.RouteManager
	igw      external.IGWManager
	topology topology.Manager
	localAZ  string
	host     string
	chassis  []string
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
	return &reconciler{
		ovn:      cfg.OVN,
		sg:       cfg.SG,
		nat:      cfg.NAT,
		routes:   cfg.Routes,
		igw:      cfg.IGW,
		topology: cfg.Topology,
		localAZ:  cfg.LocalAZ,
		host:     cfg.NodeHostname,
		chassis:  cfg.Chassis,
	}, nil
}

// Reconcile snapshots OVN actual state, computes the (intent - actual) and
// (actual - intent) deltas per resource type, and applies them in
// topologically-sorted order: VPC → Subnet → SG (port-group + ACLs) →
// Port → IGW → EIP → NATGW. Deletes run reverse: NATGW → EIP → IGW → Port
// → SG → Subnet → VPC.
//
// SG comes before Port so that ENI LSPs can be created with their port-group
// memberships atomically (CreateLogicalSwitchPortInGroups) — the racy
// create-then-join used by the legacy 5-pass startup is the exact gap this
// reconciler kills.
//
// A per-stage error is logged and the stage continues with the next item;
// the function only returns an error when the actual-state scan fails. This
// matches the legacy 5-pass behaviour: partial progress beats no progress.
func (r *reconciler) Reconcile(ctx context.Context, intent IntentState) error {
	actual, err := scanActual(ctx, r.ovn)
	if err != nil {
		return fmt.Errorf("scan actual OVN state: %w", err)
	}

	slog.Info("reconcile: starting",
		"local_az", r.localAZ,
		"intent_vpcs", len(intent.VPCs),
		"intent_subnets", len(intent.Subnets),
		"intent_ports", len(intent.Ports),
		"intent_sgs", len(intent.SGs),
		"intent_igws", len(intent.IGWs),
		"intent_eips", len(intent.EIPs),
		"intent_natgws", len(intent.NATGWs),
	)

	r.applyVPCs(ctx, intent, actual)
	r.applySubnets(ctx, intent, actual)
	r.applySGs(ctx, intent, actual)
	r.applyPorts(ctx, intent, actual)
	r.applyIGWs(ctx, intent, actual)
	r.applyEIPs(ctx, intent, actual)
	r.applyNATGWs(ctx, intent, actual)

	slog.Info("reconcile: complete")
	return nil
}
