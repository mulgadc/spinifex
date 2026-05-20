//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// phase9_NodeRecovery is the Go port of phase 9 from
// run-multinode-e2e.sh:961-1037. Starts spinifex.target on node2,
// waits for NATS to reform to 2 peers, gateway to answer, spx get
// nodes to show 3 Ready, and node2's gateway to answer
// DescribeInstanceTypes.
//
// Idempotent: if phase 8 didn't actually take node2 down (e.g.
// `go test -run TestMultinodeNodeRecovery` in isolation), the StartNode
// call no-ops and the assertions still hold.
func phase9_NodeRecovery(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode Phase 9 — Node Recovery")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3, "phase 9 requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	node2 := fix.Cluster.Nodes[1]

	harness.Step(t, "start spinifex.target on %s (%s)", node2.Name, node2.Addr)
	harness.StartNode(t, node2)

	harness.Step(t, "wait NATS to reform to 2 peers")
	fix.Cluster.WaitNATSPeers(t, 2, harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

	harness.Step(t, "wait %s gateway to answer HTTPS", node2.Name)
	harness.WaitNodeServiceReady(t, node2, harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	harness.Step(t, "spx get nodes shows %d Ready after recovery", len(fix.Cluster.Nodes))
	out := harness.SpxGetNodesAcrossCluster(t)
	harness.Detail(t, "spx_get_nodes", out)
	if ready := readyNodeCount(out); ready < len(fix.Cluster.Nodes) {
		t.Fatalf("spx get nodes: %d Ready, want >= %d after recovery\n%s",
			ready, len(fix.Cluster.Nodes), out)
	}

	harness.Step(t, "DescribeInstanceTypes answers via %s after recovery", node2.Name)
	cli := harness.AWSClientForGateway(t, fix.Env, node2)
	_, err := cli.EC2.DescribeInstanceTypes(nil)
	require.NoErrorf(t, err, "%s describe-instance-types after recovery", node2.Name)
}
