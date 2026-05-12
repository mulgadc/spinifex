//go:build e2e

// Package scenarios holds the DDIL E2E scenario tests. Each scenario
// (A..F) exercises one failure mode documented in
// docs/development/improvements/ddil-e2e-test-harness.md. Scenarios start
// as t.Skip stubs and are flipped to real assertions by the hardening
// epics that make them runnable (daemon-local-autonomy,
// predastore-ddil-hardening).
package scenarios

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/ddil/harness"
)

// scenarioLetters is the canonical ordered list of scenarios the suite
// knows about. It is used for DDIL_DRY_RUN=1 logging and by
// TestCoverageDrift as the source-of-truth set when cross-checking
// TEST_COVERAGE.md. Keep in sync with the TestScenario<L>_* functions in
// this package — TestCoverageDrift fails loudly if it drifts.
var scenarioLetters = []string{"A", "B", "C", "D", "E", "F"}

// Package-level state shared between TestMain's signal handler and
// TestHarnessSmoke. Populated by setupCluster when DDIL_NODES is set and
// DDIL_DRY_RUN is unset; both remain nil otherwise.
var (
	clusterOnce sync.Once
	cluster     *harness.Cluster
	sshXport    harness.SSH
	clusterErr  error
)

// dryRun reports whether DDIL_DRY_RUN=1 is set. Scenarios and
// TestHarnessSmoke skip cluster-touching work in dry-run mode.
func dryRun() bool { return os.Getenv("DDIL_DRY_RUN") == "1" }

// setupCluster builds the Cluster/SSH pair once per test binary. Callers
// that need cluster access gate on both (cluster, err): cluster is nil in
// dry-run or when DDIL_NODES is unset.
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

// TestMain is the suite-level entry point. It installs a signal handler
// that best-effort resets the cluster on SIGINT/SIGTERM so an aborted CI
// run does not leave iptables DROP rules or tc netem qdiscs in place,
// then delegates to go test's default runner. DDIL_DRY_RUN=1 logs the
// planned scenarios and skips cluster initialisation, leaving
// TestCoverageDrift as the only meaningful assertion.
func TestMain(m *testing.M) {
	if dryRun() {
		log.Printf("ddil: DDIL_DRY_RUN=1 — planned scenarios: %v (TestCoverageDrift will still execute)", scenarioLetters)
	}

	c, s, err := setupCluster()
	if err != nil {
		log.Printf("ddil: cluster setup failed (continuing, scenarios will skip): %v", err)
	}

	stop := installSignalHandler(c, s)
	defer stop()

	code := m.Run()
	if s != nil {
		_ = s.Close()
	}
	os.Exit(code)
}

// installSignalHandler traps SIGINT/SIGTERM and, when a cluster is
// available, calls ResetAllNodes before exiting non-zero. Without a
// cluster (dry-run or missing env) it still exits non-zero so the parent
// shell sees the signal. Returns a stop function the caller must defer to
// unregister the handler.
func installSignalHandler(c *harness.Cluster, s harness.SSH) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			log.Printf("ddil: received %s, cleaning up before exit", sig)
			if c != nil && s != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if err := harness.ResetAllNodes(ctx, c, s); err != nil {
					log.Printf("ddil: ResetAllNodes on signal: %v", err)
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

// TestHarnessSmoke exercises the reset + clean-state round trip without
// running a scenario. It satisfies the Phase 1 acceptance criterion that
// "full run against real cluster completes with AssertCleanState +
// ResetAllNodes round-tripping cleanly" and gives hardening PR authors a
// fast command for validating their tofu-cluster before touching
// scenarios (go test -run TestHarnessSmoke).
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

// scenarioSkip is the shared skip helper used by each TestScenario<L>_*
// stub. Centralising the message format keeps Phase 2/3 hardening PRs to
// a single place when they flip a scenario: delete the scenarioSkip call,
// add the real body.
func scenarioSkip(t *testing.T, letter, dep string) {
	t.Helper()
	t.Skipf("scenario %s requires %s", letter, dep)
}

// scenarioDeps bundles the live cluster handles every flipped scenario
// needs. Returned by requireLiveCluster; nil on dry-run / missing env so
// the caller's t.Skip path does not have to dereference it.
type scenarioDeps struct {
	cluster *harness.Cluster
	ssh     harness.SSH
	dc      *harness.DaemonClient
	witness *harness.Witness
}

// requireLiveCluster wires up the cluster, SSH, daemon client, and witness
// factory a flipped scenario needs. Skips (rather than fails) on dry-run,
// missing DDIL_NODES, or missing AWS_REGION so the suite can run on a
// laptop without provisioning the tofu cluster — Phase 1's smoke path.
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

// launchWitnesses launches one counter VM on each requested host, registers
// a t.Cleanup that terminates them, and returns the slice in input order.
// A launch failure fails the test rather than skipping — the harness
// retries placement up to maxPlacementAttempts internally, so a hard error
// here means the cluster cannot serve a witness at all.
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
