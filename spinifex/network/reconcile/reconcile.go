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
	)

	r.applyVPCs(ctx, intent, actual)
	r.applySubnets(ctx, intent, actual)
	r.applySGs(ctx, intent, actual, pruneOrphans)
	r.applyPorts(ctx, intent, actual)
	r.applyIGWs(ctx, intent, actual)
	r.applyEIPs(ctx, intent, actual)
	r.applyNATGWs(ctx, intent, actual)

	slog.Info("reconcile: complete")
	return nil
}
