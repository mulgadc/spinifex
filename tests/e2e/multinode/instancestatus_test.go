//go:build e2e

package multinode

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runInstanceStatusFanout asserts DescribeInstanceStatus aggregates every
// node's instances rather than only the gateway-local ones. The gateway gathers
// the per-node responses under a 3-second budget and silently returns whatever
// arrived in time, so a truncated fan-out is invisible at the API surface —
// callers see a well-formed response that is simply missing instances. This is
// the only place that failure mode is observable.
func runInstanceStatusFanout(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — DescribeInstanceStatus cross-node fan-out")

	trio := needInstanceTrio(t, fix)

	// Bracket the describe with two running-instance snapshots: an id in both
	// was running for the whole window, so its absence from the status response
	// is a truncated fan-out and not a launch or terminate racing the calls.
	before := runningInstanceIDs(t, fix)
	start := time.Now()
	out, err := fix.AWS.EC2.DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{})
	elapsed := time.Since(start)
	require.NoError(t, err, "describe-instance-status")
	after := runningInstanceIDs(t, fix)

	// Logged on every run, not just on failure: a latency drifting toward the
	// gateway's 3-second Gather budget is the leading indicator of the
	// truncation this test exists to catch.
	harness.Detail(t, "describe_latency", elapsed.Round(time.Millisecond),
		"statuses", len(out.InstanceStatuses), "gather_budget", "3s")

	reported := make(map[string]bool, len(out.InstanceStatuses))
	for _, s := range out.InstanceStatuses {
		id := aws.StringValue(s.InstanceId)
		require.NotEmpty(t, id, "describe-instance-status returned an entry with no InstanceId")
		require.Falsef(t, reported[id], "describe-instance-status returned %s twice", id)
		reported[id] = true
	}

	t.Run("EveryRunningInstanceReported", func(t *testing.T) {
		for id := range before {
			if !after[id] {
				continue
			}
			require.Truef(t, reported[id],
				"instance %s was running before and after the describe but has no status entry "+
					"(fan-out truncated by the 3s Gather budget?)", id)
		}
	})

	t.Run("SpansMultipleNodes", func(t *testing.T) {
		// Placement is resolved from the qemu process on each node, not from the
		// response under test — a gateway answering only locally would otherwise
		// look self-consistent. Only the trio is resolved: each lookup costs an
		// SSH ps per node, and the trio is the set the fixture staggers across
		// the cluster.
		placement := make(map[string]string, len(trio))
		for _, id := range trio {
			if node := harness.InstanceHostingNode(t, fix.Cluster, id); node != nil {
				placement[id] = node.Name
			}
		}
		harness.Detail(t, "trio_placement", fmt.Sprintf("%v", placement))
		if distinctNodes(placement) < 2 {
			t.Skipf("trio placed on %d node(s) (%v); scheduler colocation, not a fan-out signal",
				distinctNodes(placement), placement)
		}

		var missing []string
		nodes := make(map[string]struct{}, len(placement))
		for id, node := range placement {
			if !reported[id] {
				missing = append(missing, fmt.Sprintf("%s on %s", id, node))
				continue
			}
			nodes[node] = struct{}{}
		}
		require.GreaterOrEqualf(t, len(nodes), 2,
			"describe-instance-status returned instances from %d node(s), want at least 2 "+
				"(placement %v, missing from the response: %v)", len(nodes), placement, missing)
	})
}

// runningInstanceIDs returns the set of instance IDs DescribeInstances reports
// as running.
func runningInstanceIDs(t *testing.T, fix *Fixture) map[string]bool {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []*string{aws.String("running")},
		}},
	})
	require.NoError(t, err, "describe-instances --filter instance-state-name=running")
	ids := make(map[string]bool)
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			ids[aws.StringValue(inst.InstanceId)] = true
		}
	}
	return ids
}

// distinctNodes counts the unique node names in an instance-to-node placement map.
func distinctNodes(placement map[string]string) int {
	nodes := make(map[string]struct{}, len(placement))
	for _, node := range placement {
		nodes[node] = struct{}{}
	}
	return len(nodes)
}
