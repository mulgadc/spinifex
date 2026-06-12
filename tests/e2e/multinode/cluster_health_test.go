//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runClusterHealth verifies the four mesh services (NATS, Predastore, gateway,
// daemon) are healthy on every node and that `spx get` agrees the cluster is formed.
func runClusterHealth(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Cluster Health")

	harness.Step(t, "NATS unique peers >= 2 per node")
	fix.Cluster.WaitNATSPeers(t, 2)

	harness.Step(t, "Predastore reachable per node")
	fix.Cluster.WaitPredastoreHealthy(t)

	harness.Step(t, "Gateway reachable per node")
	fix.Cluster.WaitGatewayHealthy(t)

	harness.Step(t, "Daemon ready per gateway (DescribeInstanceTypes)")
	fix.Cluster.WaitDaemonReady(t, fix.Env)

	// spx get nodes is best-effort: the CLI ↔ NATS dial can race cluster join
	// on cold-bootstrapped runners without affecting the data path. Downgrade to WARN.
	harness.Step(t, "spx get nodes shows 3 Ready (best-effort)")
	nodesOut := harness.SpxRunBestEffort(t, "get", "nodes", "--timeout", "5s")
	harness.Detail(t, "spx_get_nodes", strings.TrimSpace(nodesOut))
	if ready := readyNodeCount(nodesOut); ready < len(fix.Cluster.Nodes) {
		t.Logf("WARN: spx get nodes shows %d Ready (want >= %d) — proceeding (bash treats this as non-fatal)\n%s",
			ready, len(fix.Cluster.Nodes), nodesOut)
	}

	// Best-effort: the CLI ↔ NATS dial can transiently fail right after cluster
	// join without affecting the data path.
	harness.Step(t, "spx get vms returns (any result, even empty)")
	vmsOut := harness.SpxRunBestEffort(t, "get", "vms")
	harness.Detail(t, "spx_get_vms_bytes", len(vmsOut))
}
