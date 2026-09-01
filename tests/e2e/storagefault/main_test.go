//go:build e2e

// Package storagefault proves what a guest does when its storage backend stops
// answering, and what a stop/start does when the seal to that backend failed.
//
// The claim is about corruption, not availability. Nobody expects a guest to
// keep serving I/O while predastore is down; the question is whether it comes
// back to a filesystem that is intact and a volume whose acknowledged writes
// are all present. Those are the two assertions here, and both are expected to
// fail against the engine as it stands.
//
// The fault is SIGSTOP rather than a stop of the unit. A stopped process holds
// its TCP connections open and sends no RST, so peers see a node that is
// present and answering nothing — which is the fault that defeats code with no
// timeout, and the one a clean shutdown cannot reproduce. SIGCONT restores it
// with no state lost, so the cluster is left exactly as it was found.
//
// It is its own package rather than a case inside single/ because it must
// freeze a cluster-wide service. Everything sharing the process would see the
// same outage, so a suite that memoizes one instance across many tests cannot
// host this without making every sibling's failure ambiguous.
package storagefault

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil && pkgFix.Harness != nil {
		if err := pkgFix.Harness.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// Fixture carries per-process state shared across this package's tests.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture
	Cluster *harness.Cluster
	SSH     harness.SSH
}

func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requireStorageFaultFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips when SPINIFEX_E2E is unset or the cluster
// is a size this suite has no defined freeze set for.
func requireStorageFaultFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		cluster, err := harness.ClusterFromEnv()
		if err != nil {
			pkgFixErr = fmt.Errorf("this suite needs node SSH to inject the fault: %w", err)
			return
		}
		sshCli, err := harness.NewSSH(cluster)
		if err != nil {
			pkgFixErr = err
			return
		}
		awsCli := harness.NewAWSClient(t, env)
		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{Env: env, AWS: awsCli, Harness: h, Cluster: cluster, SSH: sshCli}
	})
	if pkgFixErr != nil {
		t.Fatalf("storagefault fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset")
	}
	if n := len(pkgFix.Cluster.Nodes); n != 1 && n < 3 {
		t.Skipf("no defined freeze set for a %d-node cluster: 1 node is RS(1,0) and 3+ leaves a floor to break, 2 is neither", n)
	}
	return pkgFix
}

// --- Fault injection -------------------------------------------------------
//
// Every signal below is sent to a PID systemd reports as the predastore unit's
// own MainPID, and only after its /proc cmdline has been confirmed. Nothing
// here matches a process by name. That is not caution for its own sake: every
// Spinifex service is the same spx binary, so `pgrep -f spx` matches NATS and
// the daemon too, and these hosts also run unrelated QEMU guests whose comm is
// indistinguishable from the platform's own.

// predastoreUnit is the systemd unit whose MainPID is the only process this
// suite ever signals.
const predastoreUnit = "spinifex-predastore"

// predastoreCmdline is the substring the resolved PID's cmdline must contain.
// A MainPID that does not match is a unit that has been redefined, and the
// safe response is to refuse rather than to signal an unknown process.
const predastoreCmdline = "service predastore"

// freezeSettle is how long to wait after signalling before treating the state
// change as in effect, so an assertion cannot race the signal.
const freezeSettle = 2 * time.Second

// predastorePID resolves the predastore unit's MainPID on node and confirms the
// process is the one expected. Returns 0 with no error when the unit is not
// running, which is a fact the caller decides what to do about.
func predastorePID(ctx context.Context, ssh harness.SSH, node harness.Node) (int, error) {
	raw, err := ssh.Run(ctx, node, "systemctl show "+predastoreUnit+" -p MainPID --value")
	if err != nil {
		return 0, fmt.Errorf("resolve %s MainPID on %s: %w", predastoreUnit, node.Name, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("MainPID on %s was %q, not a number", node.Name, strings.TrimSpace(string(raw)))
	}
	if pid <= 1 {
		return 0, nil
	}

	cmdline, err := ssh.Run(ctx, node, fmt.Sprintf("tr '\\0' ' ' < /proc/%d/cmdline", pid))
	if err != nil {
		return 0, fmt.Errorf("read cmdline of pid %d on %s: %w", pid, node.Name, err)
	}
	if !strings.Contains(string(cmdline), predastoreCmdline) {
		return 0, fmt.Errorf("refusing to signal pid %d on %s: cmdline %q does not contain %q",
			pid, node.Name, strings.TrimSpace(string(cmdline)), predastoreCmdline)
	}
	return pid, nil
}

// predastoreStopped reports whether the predastore process on node is in state
// T. Used both to refuse a run that starts against a frozen cluster and to
// confirm a thaw actually took.
func predastoreStopped(ctx context.Context, ssh harness.SSH, node harness.Node) (bool, error) {
	pid, err := predastorePID(ctx, ssh, node)
	if err != nil || pid == 0 {
		return false, err
	}
	state, err := ssh.Run(ctx, node, fmt.Sprintf("ps -o state= -p %d", pid))
	if err != nil {
		return false, fmt.Errorf("read state of pid %d on %s: %w", pid, node.Name, err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(state)), "T"), nil
}

// requireNoneFrozen fails the run when any node's predastore is already
// stopped. A leftover from an aborted run would make every assertion below
// meaningless, and thawing someone else's fault injection is not this suite's
// call to make.
func requireNoneFrozen(t *testing.T, f *Fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, n := range f.Cluster.Nodes {
		stopped, err := predastoreStopped(ctx, f.SSH, n)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		if stopped {
			t.Fatalf("preflight: predastore on %s is already stopped — a previous run left it frozen. "+
				"Thaw it with: ssh %s 'sudo kill -CONT $(systemctl show %s -p MainPID --value)'",
				n.Name, n.Addr, predastoreUnit)
		}
	}
}

// freezePredastore SIGSTOPs predastore on each node and registers an
// unconditional thaw. The cleanup is registered before the first signal, so a
// failure part-way through the set still restores the nodes already frozen.
func freezePredastore(t *testing.T, f *Fixture, nodes []harness.Node) {
	t.Helper()
	t.Cleanup(func() { thawPredastore(t, f, nodes) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, n := range nodes {
		pid, err := predastorePID(ctx, f.SSH, n)
		if err != nil {
			t.Fatalf("freeze: %v", err)
		}
		if pid == 0 {
			t.Fatalf("freeze: %s is not running on %s", predastoreUnit, n.Name)
		}
		if _, err := f.SSH.Run(ctx, n, fmt.Sprintf("sudo kill -STOP %d", pid)); err != nil {
			t.Fatalf("freeze: SIGSTOP pid %d on %s: %v", pid, n.Name, err)
		}
		t.Logf("froze predastore pid %d on %s", pid, n.Name)
	}
	time.Sleep(freezeSettle)
}

// thawPredastore SIGCONTs predastore on each node and confirms it left state T.
// A node that cannot be thawed fails the test even if every assertion passed:
// leaving one frozen wedges the cluster for whatever runs next.
func thawPredastore(t *testing.T, f *Fixture, nodes []harness.Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, n := range nodes {
		pid, err := predastorePID(ctx, f.SSH, n)
		if err != nil || pid == 0 {
			t.Errorf("thaw: cannot resolve predastore on %s (pid=%d): %v", n.Name, pid, err)
			continue
		}
		if _, err := f.SSH.Run(ctx, n, fmt.Sprintf("sudo kill -CONT %d", pid)); err != nil {
			t.Errorf("thaw: SIGCONT pid %d on %s: %v", pid, n.Name, err)
			continue
		}
		stopped, err := predastoreStopped(ctx, f.SSH, n)
		if err != nil {
			t.Errorf("thaw: confirm %s: %v", n.Name, err)
			continue
		}
		if stopped {
			t.Errorf("thaw: predastore on %s is still stopped after SIGCONT", n.Name)
			continue
		}
		t.Logf("thawed predastore pid %d on %s", pid, n.Name)
	}
	time.Sleep(freezeSettle)
}

// freezeSetFor returns the nodes to freeze so the backend is definitively below
// its floor for a guest on hostNode, and a label describing why.
//
// Every node but the guest's own is frozen. On three nodes that is two of
// three: one shard is left, below DataShards=2 with degraded writes on, and
// meta raft loses quorum. Leaving the guest's own node up is deliberate — the
// local gate stays reachable, so what is being tested is the backend having no
// quorum rather than viperblock having no gate to talk to.
func freezeSetFor(f *Fixture, hostNode *harness.Node) ([]harness.Node, string) {
	all := f.Cluster.Nodes
	if len(all) == 1 {
		return all, "the only node: RS(1,0) has no redundancy, so any freeze is total"
	}

	var out []harness.Node
	for _, n := range all {
		if hostNode != nil && n.Index == hostNode.Index {
			continue
		}
		out = append(out, n)
	}
	return out, fmt.Sprintf("%d of %d nodes, every one but the guest's own host", len(out), len(all))
}
