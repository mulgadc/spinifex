//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runNodeFailure is the Go port of node-failure injection
// (run-multinode-e2e.sh:898-957). Stops spinifex.target on node2, asserts the
// cluster degrades cleanly: surviving nodes still serve DescribeInstances,
// NATS reports 1 peer instead of 2, and the trio remains addressable.
//
// Registers a t.Cleanup that restarts node2 unconditionally so a cancelled
// recovery test doesn't leave the cluster broken for downstream tests.
// runNodeRecovery itself runs the recovery assertions explicitly.
func runNodeFailure(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Node Failure")

	// Trio must already exist — node2 going down shouldn't cause a race with
	// trio launches issuing through that node's gateway.
	_ = needInstanceTrio(t, fix)
	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3, "node failure requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	node2 := fix.Cluster.Nodes[1]
	node3 := fix.Cluster.Nodes[2]
	local := fix.Cluster.Nodes[0]

	harness.Step(t, "stop spinifex.target on %s (%s)", node2.Name, node2.Addr)
	harness.StopNode(t, node2)
	t.Cleanup(func() {
		// Best-effort: ensure node2 comes back even if assertions below fail
		// or the run is cancelled. runNodeRecovery calls StartNode again —
		// systemctl start is idempotent.
		harness.StartNode(t, node2)
	})

	// NATS detection of a peer drop is delayed by the heartbeat — bash
	// uses `sleep 10`. WaitNATSPeers polls so we converge faster on a
	// healthy run while still tolerating a slow detection.
	harness.Step(t, "wait NATS to report 1 peer (degraded)")
	fix.Cluster.WaitNATSPeers(t, 1, harness.WithTimeout(30*time.Second), harness.WithPoll(2*time.Second))

	harness.Step(t, "DescribeInstanceTypes still answers via %s", local.Name)
	localCli := harness.AWSClientForGateway(t, fix.Env, local)
	_, err := localCli.EC2.DescribeInstanceTypes(nil)
	require.NoErrorf(t, err, "%s describe-instance-types during node2 failure", local.Name)

	harness.Step(t, "DescribeInstanceTypes still answers via %s", node3.Name)
	node3Cli := harness.AWSClientForGateway(t, fix.Env, node3)
	_, err = node3Cli.EC2.DescribeInstanceTypes(nil)
	require.NoErrorf(t, err, "%s describe-instance-types during node2 failure", node3.Name)

	harness.Step(t, "DescribeInstances still returns >0 from %s", local.Name)
	out, err := localCli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "describe-instances during failure")
	total := 0
	for _, r := range out.Reservations {
		total += len(r.Instances)
	}
	harness.Detail(t, "instances_visible", total)
	require.Greaterf(t, total, 0, "no instances visible from %s during node2 failure", local.Name)
}
