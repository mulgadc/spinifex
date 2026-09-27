//go:build e2e

// Package instancerecovery proves what happens to a running guest when the
// node under it stops answering, from both sides: a host failure moves it, and
// a storage failure must not.
//
// It is its own suite for the reason storagefault is: both tests take a node
// away and one of them freezes a cluster-wide service, so anything sharing the
// environment would see the same outage and report a failure that says nothing
// about itself. It is also slow by nature — a guest has to be seen stale, then
// claimed, then booted, and the storage case has to wait minutes for a pause
// that is not prompt — which is why it does not belong in a permutation cell
// budgeted at thirty-five minutes.
package instancerecovery

import (
	"fmt"
	"os"
	"sync"
	"testing"

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
}

// requireFixture returns the package-scoped Fixture, building it on first
// call. Skips below three nodes, where recovery is inert by design: there is
// no survivor to move a guest to that is not the node that failed.
func requireFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		cluster, err := harness.ClusterFromEnv()
		if err != nil {
			pkgFixErr = fmt.Errorf("this suite needs node SSH to take a node away: %w", err)
			return
		}
		awsCli := harness.NewAWSClient(t, env)
		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{Env: env, AWS: awsCli, Harness: h, Cluster: cluster}
	})
	if pkgFixErr != nil {
		t.Fatalf("instancerecovery fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset")
	}
	if n := len(pkgFix.Cluster.Nodes); n < 3 {
		t.Skipf("recovery is inert below three nodes, have %d", n)
	}
	return pkgFix
}

// TestInstanceAutoRecovery is sequential and runs first: it leaves the cluster
// whole again before the storage case takes a node away for a second time.
func TestInstanceAutoRecovery(t *testing.T) {
	runInstanceAutoRecovery(t, requireFixture(t))
}

// TestInstanceRecoveryRefusesStorageFault is sequential and declared after the
// recovery test, because it asserts the opposite outcome and a reconciler that
// cannot tell the two faults apart passes one and fails the other.
func TestInstanceRecoveryRefusesStorageFault(t *testing.T) {
	runInstanceRecoveryRefusesStorageFault(t, requireFixture(t))
}

// TestInstancePartitionRecovery is last because it is the only one that leaves
// firewall state behind if it fails, and running it after the other two means a
// failure here cannot be mistaken for the cause of theirs.
func TestInstancePartitionRecovery(t *testing.T) {
	runInstancePartitionRecovery(t, requireFixture(t))
}
