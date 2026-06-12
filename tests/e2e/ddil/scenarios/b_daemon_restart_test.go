//go:build e2e

package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/ddil/fault"
	ddilh "github.com/mulgadc/spinifex/tests/e2e/ddil/harness"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// TestScenarioB_DaemonRestartWithoutNATS kills spinifex-nats, restarts spinifex-daemon, and
// verifies the daemon enters standalone mode within 30s and recovers its instances from the
// local state file. PID/command-line identity is not asserted; recovery + live QEMU is sufficient.
func TestScenarioB_DaemonRestartWithoutNATS(t *testing.T) {
	deps := requireLiveCluster(t)
	c, ssh, dc, w := deps.cluster, deps.ssh, deps.dc, deps.witness

	ddilh.Run(t, c, ssh, "B", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		harness.AssertCleanState(ctx, t, c, ssh)

		node3 := c.Nodes[2]
		_ = launchWitnesses(ctx, t, w, node3)

		pre, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("pre-fault snapshot on %s: %v", node3.Name, err)
		}
		if len(pre) == 0 {
			t.Fatalf("pre-fault /local/instances on %s empty — witness launch did not register", node3.Name)
		}

		if err := fault.KillNATS(ctx, ssh, node3); err != nil {
			t.Fatalf("kill nats on %s: %v", node3.Name, err)
		}
		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer ccancel()
			_ = fault.StartNATS(cctx, ssh, node3)
		})

		// 30s budget: if the daemon misses it, the startup path has regressed to the 5-minute NATS-wait abort.
		restartStart := time.Now()
		if err := fault.RestartDaemonOnly(ctx, ssh, node3); err != nil {
			t.Fatalf("restart daemon on %s: %v", node3.Name, err)
		}
		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeStandalone, 30*time.Second); err != nil {
			t.Fatalf("%s did not enter standalone within 30s of restart: %v", node3.Name, err)
		}
		t.Logf("daemon on %s reached standalone in %s", node3.Name, time.Since(restartStart))

		post, err := fault.TakeSnapshot(ctx, dc, node3)
		if err != nil {
			t.Fatalf("post-restart snapshot on %s: %v", node3.Name, err)
		}
		pre.AssertPreserved(t, post)

		// Non-zero PID proves the daemon re-attached to the QEMU pidfile after restart.
		for _, inst := range post {
			if inst.PID == 0 {
				t.Errorf("recovered instance %s has no live QEMU PID", inst.InstanceID)
			}
		}

		if err := fault.StartNATS(ctx, ssh, node3); err != nil {
			t.Fatalf("restart nats on %s: %v", node3.Name, err)
		}
		if err := harness.WaitForMode(ctx, dc, node3, harness.ModeCluster, 60*time.Second); err != nil {
			t.Fatalf("%s did not rejoin cluster after NATS restart: %v", node3.Name, err)
		}
	})
}
