//go:build e2e

package multinode

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runCrossNodeGateway drives DescribeInstances through every node's gateway and asserts
// each returns the same instance count as node1. Catches the regression where the daemon
// answers only locally-hosted instances instead of fanning out via NATS.
func runCrossNodeGateway(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Cross-Node Gateway Access")

	// Trigger trio launch so the baseline count is meaningful.
	_ = needInstanceTrio(t, fix)

	baseline, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "baseline describe-instances")
	want := countInstances(baseline)
	harness.Detail(t, "baseline_node", fix.Cluster.Nodes[0].Name, "instances", want)
	require.Greater(t, want, 0, "baseline returned 0 instances (trio not registered?)")

	// Bash skips node1 (the local one) — its baseline already represents that gateway.
	for _, n := range fix.Cluster.Nodes[1:] {
		harness.Step(t, "DescribeInstances via %s (%s)", n.Name, n.Addr)
		cli := harness.AWSClientForGateway(t, fix.Env, n)
		out, err := cli.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
		require.NoErrorf(t, err, "describe-instances via %s", n.Name)
		got := countInstances(out)
		harness.Detail(t, "node", n.Name, "instances", got)
		require.Equalf(t, want, got, "%s returned %d instances, baseline %d (cross-node fan-out broken)", n.Name, got, want)
	}
}

func countInstances(out *ec2.DescribeInstancesOutput) int {
	n := 0
	for _, r := range out.Reservations {
		n += len(r.Instances)
	}
	return n
}
