//go:build e2e

package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	ddilh "github.com/mulgadc/spinifex/tests/e2e/ddil/harness"
	"github.com/mulgadc/spinifex/tests/e2e/ddil/fault"
)

// TestScenarioC_CleanPartition — iptables-DROP node3 away from node1 and
// node2, verify the majority keeps serving API, the isolated node
// reports standalone mode, and heal converges state without duplicate
// or orphaned VMs. See
// docs/development/improvements/ddil-e2e-test-harness.md §3 Scenario C.
func TestScenarioC_CleanPartition(t *testing.T) {
	deps := requireLiveCluster(t)
	c, ssh, dc, w := deps.cluster, deps.ssh, deps.dc, deps.witness

	ddilh.Run(t, c, ssh, "C", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		harness.AssertCleanState(ctx, t, c, ssh)

		node1, node2, node3 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
		// Witness on node3 proves the partitioned-side workload kept
		// running. Counters elsewhere are not asserted: §3 step 6 names
		// only node-3.
		witnesses := launchWitnesses(ctx, t, w, node3)

		pre, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("pre-fault snapshot on %s: %v", node3.Name, err)
		}

		if err := fault.PartitionNode(ctx, ssh, node3, c.Peers(node3)); err != nil {
			t.Fatalf("partition %s: %v", node3.Name, err)
		}
		// HealNode flushes node3's iptables on cleanup; safe even if a later
		// step already healed because HealNode is idempotent.
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer ccancel()
			_ = fault.HealNode(cctx, ssh, node3)
		})

		// The orchestrator dials peers directly from outside the cluster
		// network, so each side stays reachable for assertions even though
		// the peers can no longer reach each other.
		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeStandalone, 30*time.Second); err != nil {
			t.Fatalf("%s did not enter standalone after partition: %v", node3.Name, err)
		}

		// Majority side: both daemons remain reachable and continue serving
		// /local/instances independently of NATS routing to node3.
		for _, n := range []harness.Node{node1, node2} {
			if _, err := dc.Health(ctx, n); err != nil {
				t.Fatalf("majority %s /health: %v", n.Name, err)
			}
			if _, err := dc.Instances(ctx, n); err != nil {
				t.Fatalf("majority %s /local/instances: %v", n.Name, err)
			}
		}
		// Isolated side: /local/* keeps responding with the daemon's local
		// view even with no NATS reachability to peers.
		if _, err := dc.Instances(ctx, node3); err != nil {
			t.Fatalf("isolated %s /local/instances: %v", node3.Name, err)
		}

		// Witness on the partitioned side must keep advancing its counter,
		// proving QEMU on node3 kept executing despite the network split.
		harness.AssertProgressed(ctx, t, witnesses[0])

		if err := fault.HealNode(ctx, ssh, node3); err != nil {
			t.Fatalf("heal %s: %v", node3.Name, err)
		}
		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeCluster, 60*time.Second); err != nil {
			t.Fatalf("%s did not return to cluster mode after heal: %v", node3.Name, err)
		}

		post, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("post-heal snapshot on %s: %v", node3.Name, err)
		}
		pre.AssertPreserved(t, post)
	})
}
