//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runNodeFailure simulates a hard outage on node2 (stops its spinifex services,
// guests keep running) and asserts clean degradation: surviving nodes still
// serve DescribeInstances and NATS reports 1 peer. Unconditionally restarts
// node2 in t.Cleanup so downstream tests are unaffected.
func runNodeFailure(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Node Failure")

	// Trio must already exist — node2 going down shouldn't cause a race with
	// trio launches issuing through that node's gateway.
	_ = needInstanceTrio(t, fix)
	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3, "node failure requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	node2 := fix.Cluster.Nodes[1]
	node3 := fix.Cluster.Nodes[2]
	local := fix.Cluster.Nodes[0]

	harness.Step(t, "stop spinifex services on %s (%s) — guests stay running", node2.Name, node2.Addr)
	harness.StopNode(t, node2)
	t.Cleanup(func() {
		harness.StartNode(t, node2) // idempotent; runNodeRecovery may also call this
	})

	// NATS peer-drop detection is delayed by heartbeat; poll so we converge faster than a fixed sleep.
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
