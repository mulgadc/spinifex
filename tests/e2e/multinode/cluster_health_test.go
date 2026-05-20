//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// phase2_ClusterHealth is the Go port of `Phase 2: Cluster Health`
// (run-multinode-e2e.sh:426-510). Verifies the four mesh services
// (NATS, Predastore, gateway, daemon) are healthy on every node and
// the `spx get` CLI agrees the cluster is formed.
//
// Bash version uses `pass_test` / `fail_test` to accumulate per-check
// pass counts; the Go port lets each failing sub-check fail the whole
// test (t.Fatalf) — a single dead node should fail the phase fast.
func phase2_ClusterHealth(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode Phase 2 — Cluster Health")

	harness.Step(t, "NATS unique peers >= 2 per node")
	fix.Cluster.WaitNATSPeers(t, 2)

	harness.Step(t, "Predastore reachable per node")
	fix.Cluster.WaitPredastoreHealthy(t)

	harness.Step(t, "Gateway reachable per node")
	fix.Cluster.WaitGatewayHealthy(t)

	harness.Step(t, "Daemon ready per gateway (DescribeInstanceTypes)")
	fix.Cluster.WaitDaemonReady(t, fix.Env)

	harness.Step(t, "spx get nodes shows 3 Ready")
	nodesOut := harness.SpxGetNodes(t)
	harness.Detail(t, "spx_get_nodes", strings.TrimSpace(nodesOut))
	if ready := readyNodeCount(nodesOut); ready < len(fix.Cluster.Nodes) {
		t.Fatalf("spx get nodes: %d Ready, want >= %d\n%s", ready, len(fix.Cluster.Nodes), nodesOut)
	}

	// Best-effort: bash phase 2 runs `spx get vms --timeout 5s 2>/dev/null`
	// and never checks the exit code — the CLI ↔ NATS dial can transiently
	// fail right after cluster join without affecting the data path.
	harness.Step(t, "spx get vms returns (any result, even empty)")
	vmsOut := harness.SpxRunBestEffort(t, "get", "vms")
	harness.Detail(t, "spx_get_vms_bytes", len(vmsOut))
}
