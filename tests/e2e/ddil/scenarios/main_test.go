//go:build e2e

// Package scenarios holds the DDIL E2E scenario tests (A..F), each exercising one
// failure mode. Scenarios start as t.Skip stubs and are flipped to real assertions
// by the hardening epics that make them runnable.
package scenarios

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// scenarioLetters is the canonical scenario list used for DDIL_DRY_RUN logging and
// TestCoverageDrift cross-checking. Keep in sync with TestScenario<L>_* functions.
var scenarioLetters = []string{"A", "B", "C", "D", "E", "F"}

// Package-level cluster state. Populated by setupCluster when DDIL_NODES is set; nil in dry-run.
var (
	clusterOnce sync.Once
	cluster     *harness.Cluster
	sshXport    harness.SSH
	clusterErr  error
)

// dryRun reports whether DDIL_DRY_RUN=1 is set. Scenarios and
// TestHarnessSmoke skip cluster-touching work in dry-run mode.
func dryRun() bool { return os.Getenv("DDIL_DRY_RUN") == "1" }

// setupCluster builds the Cluster/SSH pair once. Returns nil cluster in dry-run or when DDIL_NODES is unset.
func setupCluster() (*harness.Cluster, harness.SSH, error) {
	clusterOnce.Do(func() {
		if dryRun() {
			return
		}
		if os.Getenv("DDIL_NODES") == "" {
			return
		}
		c, err := harness.ClusterFromEnv()
		if err != nil {
			clusterErr = err
			return
		}
		s, err := harness.NewSSH(c)
		if err != nil {
			clusterErr = err
			return
		}
		cluster = c
		sshXport = s
	})
	return cluster, sshXport, clusterErr
}

// TestMain installs a SIGINT/SIGTERM handler that resets the cluster before exit
// (clears iptables DROP rules and tc netem qdiscs), then runs the suite.
// DDIL_DRY_RUN=1 skips cluster init and leaves only TestCoverageDrift active.
func TestMain(m *testing.M) {
	if dryRun() {
		slog.Info("ddil: DDIL_DRY_RUN=1, TestCoverageDrift will still execute", "planned_scenarios", scenarioLetters)
	}

	c, s, err := setupCluster()
	if err != nil {
		slog.Warn("ddil: cluster setup failed, continuing and scenarios will skip", "error", err)
	}

	stop := installSignalHandler(c, s)
	defer stop()

	code := m.Run()
	if s != nil {
		_ = s.Close()
	}
	os.Exit(code)
}

// installSignalHandler traps SIGINT/SIGTERM; calls ResetAllNodes when a cluster is
// available, then exits non-zero. Returns a stop func the caller must defer.
func installSignalHandler(c *harness.Cluster, s harness.SSH) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			slog.Info("ddil: received signal, cleaning up before exit", "signal", sig)
			if c != nil && s != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if err := harness.ResetAllNodes(ctx, c, s); err != nil {
					slog.Error("ddil: ResetAllNodes on signal failed", "error", err)
				}
				_ = s.Close()
			}
			os.Exit(1)
		case <-done:
			return
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// TestHarnessSmoke exercises the ResetAllNodes + AssertCleanState round trip without
// running a scenario. Fast validation command for a live cluster.
func TestHarnessSmoke(t *testing.T) {
	if dryRun() {
		t.Skipf("DDIL_DRY_RUN=1")
	}
	c, s, err := setupCluster()
	if err != nil {
		t.Fatalf("cluster setup: %v", err)
	}
	if c == nil || s == nil {
		t.Skipf("DDIL_NODES unset (harness smoke requires a live cluster)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := harness.ResetAllNodes(ctx, c, s); err != nil {
		t.Fatalf("ResetAllNodes: %v", err)
	}
	harness.AssertCleanState(ctx, t, c, s)
}

// scenarioSkip is the shared skip helper for scenario stubs.
func scenarioSkip(t *testing.T, letter, dep string) {
	t.Helper()
	t.Skipf("scenario %s requires %s", letter, dep)
}

// scenarioDeps bundles live cluster handles needed by flipped scenarios.
type scenarioDeps struct {
	cluster *harness.Cluster
	ssh     harness.SSH
	dc      *harness.DaemonClient
	witness *harness.Witness
}

// requireLiveCluster wires up cluster, SSH, daemon client, and witness factory.
// Skips on dry-run, missing DDIL_NODES, or insufficient cluster size.
func requireLiveCluster(t *testing.T) scenarioDeps {
	t.Helper()
	if dryRun() {
		t.Skipf("DDIL_DRY_RUN=1")
	}
	c, s, err := setupCluster()
	if err != nil {
		t.Fatalf("cluster setup: %v", err)
	}
	if c == nil || s == nil {
		t.Skipf("DDIL_NODES unset (scenario requires a live cluster)")
	}
	if len(c.Nodes) < 3 {
		t.Skipf("scenario requires 3-node cluster, got %d", len(c.Nodes))
	}
	w, err := harness.NewWitness(c, s)
	if err != nil {
		t.Skipf("witness setup: %v", err)
	}
	return scenarioDeps{
		cluster: c,
		ssh:     s,
		dc:      harness.NewDaemonClient(),
		witness: w,
	}
}

// launchWitnesses launches one counter VM per host, registers t.Cleanup termination,
// and returns VMs in input order. Fatals on launch failure (placement retries are internal).
func launchWitnesses(ctx context.Context, t *testing.T, w *harness.Witness, hosts ...harness.Node) []*harness.WitnessVM {
	t.Helper()
	out := make([]*harness.WitnessVM, 0, len(hosts))
	for _, h := range hosts {
		v, err := harness.LaunchWitnessVM(ctx, w, h)
		if err != nil {
			t.Fatalf("launch witness on %s: %v", h.Name, err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()
			if err := v.Terminate(cctx); err != nil {
				t.Logf("terminate witness %s: %v", v.InstanceID, err)
			}
		})
		out = append(out, v)
	}
	return out
}
