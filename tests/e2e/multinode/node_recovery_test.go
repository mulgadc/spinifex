//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runNodeRecovery starts spinifex.target on node2 and asserts full cluster reformation:
// NATS 2 peers, gateway answering, and DescribeInstanceTypes succeeding via node2.
// Idempotent if node2 was never stopped.
func runNodeRecovery(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Node Recovery")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3, "node recovery requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	node2 := fix.Cluster.Nodes[1]

	harness.Step(t, "start spinifex.target on %s (%s)", node2.Name, node2.Addr)
	harness.StartNode(t, node2)

	harness.Step(t, "wait NATS to reform to 2 peers")
	fix.Cluster.WaitNATSPeers(t, 2, harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

	harness.Step(t, "wait %s gateway to answer HTTPS", node2.Name)
	harness.WaitNodeServiceReady(t, node2, harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	// spx get nodes is best-effort; real recovery gate is DescribeInstanceTypes via node2.
	harness.Step(t, "spx get nodes shows %d Ready after recovery (best-effort)", len(fix.Cluster.Nodes))
	out := harness.SpxGetNodesAcrossCluster(t)
	harness.Detail(t, "spx_get_nodes", out)
	if ready := readyNodeCount(out); ready < len(fix.Cluster.Nodes) {
		t.Logf("WARN: spx get nodes shows %d Ready after recovery (want >= %d) — proceeding (bash treats this as non-fatal)\n%s",
			ready, len(fix.Cluster.Nodes), out)
	}

	harness.Step(t, "DescribeInstanceTypes answers via %s after recovery", node2.Name)
	cli := harness.AWSClientForGateway(t, fix.Env, node2)
	_, err := cli.EC2.DescribeInstanceTypes(nil)
	require.NoErrorf(t, err, "%s describe-instance-types after recovery", node2.Name)
}
