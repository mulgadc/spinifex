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

// TestScenarioB_DaemonRestartWithoutNATS — kill spinifex-nats, restart
// spinifex-daemon, verify the daemon starts within 30s (not the old
// 5-minute NATS-wait abort) and recovers its instances from the local
// state file. See
// docs/development/improvements/ddil-e2e-test-harness.md §3 Scenario B.
//
// Plan §3 Scenario B step 8 originally asked for "same PID, same command
// line" across the restart. Operationally only recovery matters — PID is
// just a label and would only stay stable if the daemon unit set
// KillMode=process (today it uses systemd's default control-group).
// Asserting recovery + a live post-restart QEMU process gives the same
// regression coverage without coupling the test to the unit file.
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

		// 1d's promise is "daemon comes up without NATS". The 30s budget is
		// the plan's threshold — if it slips, the new startup path has
		// regressed back to the 5-minute NATS-wait abort.
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

		// Each recovered instance must have a live QEMU process. /local/instances
		// reports PID from utils.ReadPidFile (local_api.go:112) — a non-zero
		// value proves the daemon found and re-attached to (or relaunched)
		// the QEMU pidfile. Zero means no QEMU on this host.
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
