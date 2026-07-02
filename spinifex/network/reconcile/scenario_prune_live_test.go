package reconcile

// Orphan-prune scenario: create a VPC/subnet/SG/ENI, then reconcile an intent
// that no longer contains the ENI or SG and assert the reconciler tears the NB
// rows down. It also locks the ReconcileApplyOnly startup-race guard: apply-only
// must NOT prune, full Reconcile must. Missed teardown is a top source of stale
// NB state, and the whole prune path is otherwise only covered against ovn/mock.

import (
	"context"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func TestScenario_OrphanPrune_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	ctx := context.Background()

	// 1. Converge the full state and capture the ENI LSP UUID.
	if err := rec.Reconcile(ctx, freshIntent(t)); err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}
	pgName := topology.SecurityGroupPortGroup("sg-a")
	eni, err := cli.GetLogicalSwitchPort(ctx, "port-eni-a")
	if err != nil {
		t.Fatalf("precondition: ENI port absent after create: %v", err)
	}
	eniUUID := eni.UUID
	if _, err := cli.GetPortGroup(ctx, pgName); err != nil {
		t.Fatalf("precondition: SG port group absent after create: %v", err)
	}

	// 2. Intent with the ENI and SG removed (VPC + subnet retained).
	reduced := freshIntent(t)
	reduced.Ports = map[string]topology.PortSpec{}
	reduced.SGs = map[string]policy.SGSpec{}

	// 3. ReconcileApplyOnly must NOT prune (startup-race guard).
	if err := rec.ReconcileApplyOnly(ctx, reduced); err != nil {
		t.Fatalf("ReconcileApplyOnly: %v", err)
	}
	if _, err := cli.GetLogicalSwitchPort(ctx, "port-eni-a"); err != nil {
		t.Errorf("ApplyOnly pruned the ENI port; startup-race guard broken: %v", err)
	}
	if _, err := cli.GetPortGroup(ctx, pgName); err != nil {
		t.Errorf("ApplyOnly pruned the SG port group; startup-race guard broken: %v", err)
	}

	// 4. Full Reconcile must prune both.
	if err := rec.Reconcile(ctx, reduced); err != nil {
		t.Fatalf("Reconcile (prune): %v", err)
	}
	if _, err := cli.GetLogicalSwitchPort(ctx, "port-eni-a"); err == nil {
		t.Errorf("orphan ENI port not pruned")
	}
	if _, err := cli.GetPortGroup(ctx, pgName); err == nil {
		t.Errorf("orphan SG port group not pruned")
	}
	ls, err := cli.GetLogicalSwitch(ctx, "subnet-subnet-a")
	if err != nil {
		t.Fatalf("GetLogicalSwitch after prune: %v", err)
	}
	if slices.Contains(ls.Ports, eniUUID) {
		t.Errorf("subnet switch still references pruned ENI port %s", eniUUID)
	}

	// 5. Prune is idempotent: a second pass leaves NB unchanged.
	before := snapshotNB(t, ctx, cli)
	if err := rec.Reconcile(ctx, reduced); err != nil {
		t.Fatalf("Reconcile (prune #2): %v", err)
	}
	after := snapshotNB(t, ctx, cli)
	if before != after {
		t.Fatalf("prune not idempotent; NB changed on second pass:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
