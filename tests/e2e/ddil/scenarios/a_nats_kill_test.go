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

// TestScenarioA_NATSKill — stop spinifex-nats on a single node without
// touching spinifex-daemon, verify the daemon stays up in standalone
// mode, serves its local API, and the surviving 2-node quorum keeps
// serving cluster-wide requests. See
// docs/development/improvements/ddil-e2e-test-harness.md §3 Scenario A.
func TestScenarioA_NATSKill(t *testing.T) {
	deps := requireLiveCluster(t)
	c, ssh, dc, w := deps.cluster, deps.ssh, deps.dc, deps.witness

	ddilh.Run(t, c, ssh, "A", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		harness.AssertCleanState(ctx, t, c, ssh)

		node1, node2, node3 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
		witnesses := launchWitnesses(ctx, t, w, node1, node2, node3)

		pre, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("pre-fault snapshot on %s: %v", node3.Name, err)
		}

		if err := fault.KillNATS(ctx, ssh, node3); err != nil {
			t.Fatalf("kill nats on %s: %v", node3.Name, err)
		}
		// Restore NATS in cleanup so a mid-scenario failure does not leave
		// the cluster split for the next attempt or the next scenario.
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer ccancel()
			_ = fault.StartNATS(cctx, ssh, node3)
		})

		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeStandalone, 30*time.Second); err != nil {
			t.Fatalf("%s did not enter standalone: %v", node3.Name, err)
		}

		// /local/instances on the isolated node still serves and reports
		// at least the instance set the daemon knew about pre-fault.
		local3, err := dc.Instances(ctx, node3)
		if err != nil {
			t.Fatalf("%s /local/instances: %v", node3.Name, err)
		}
		if len(local3) < len(pre) {
			t.Fatalf("%s /local/instances regressed: pre=%d post=%d", node3.Name, len(pre), len(local3))
		}

		// Surviving 2-node quorum: each majority daemon's HTTPS listener is
		// reachable and reports healthy. /health is NATS-independent so
		// this proves the daemons are up; the witness counters below prove
		// the data plane (gateway → daemon → QEMU) is still progressing.
		for _, n := range []harness.Node{node1, node2} {
			if _, err := dc.Health(ctx, n); err != nil {
				t.Fatalf("majority %s /health: %v", n.Name, err)
			}
		}

		if err := fault.StartNATS(ctx, ssh, node3); err != nil {
			t.Fatalf("restart nats on %s: %v", node3.Name, err)
		}
		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeCluster, 60*time.Second); err != nil {
			t.Fatalf("%s did not return to cluster mode: %v", node3.Name, err)
		}

		post, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("post-heal snapshot on %s: %v", node3.Name, err)
		}
		pre.AssertPreserved(t, post)

		for _, v := range witnesses {
			harness.AssertProgressed(ctx, t, v)
		}
	})
}
