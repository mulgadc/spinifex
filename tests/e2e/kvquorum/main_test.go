//go:build e2e

// Package kvquorum proves that the cluster's own state survives losing a node.
//
// Every control-plane service keeps its state in a JetStream KV bucket, and a
// bucket is replicated across however many nodes it was created with. At one
// replica it lives on one JetStream-chosen server recorded in no config, so
// losing that node makes the bucket unreadable and unwritable from every node
// at once. Leader leases are the worst case: nobody can acquire one whose
// bucket has no quorum, so every reconciler sharing it stops cluster-wide at
// exactly the moment a node has failed.
//
// It is its own suite because it takes a node away, so anything sharing the
// environment would report that outage as its own failure, and because the
// question it answers — "is our internal state actually replicated" — has to
// read as itself rather than as one more red subtest somewhere else.
package kvquorum

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// How long a survivor is given to answer for every bucket after a node
	// goes away. A raft group only has to elect a new leader for the streams
	// the departed node led, which is seconds on a healthy cluster; the budget
	// is generous because a failure here is the whole point of the suite and a
	// flake would be read as one.
	quorumRecoveryBudget = 2 * time.Minute

	// How long the returning node is given to rejoin and carry its share again.
	rejoinBudget = 5 * time.Minute

	// How often a survivor is polled while quorum is expected to return.
	pollInterval = 5 * time.Second
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

// requireFixture returns the package-scoped Fixture, building it on first call.
//
// Three nodes is the floor. On two, a majority is two, so losing either one
// leaves no quorum for any stream and there is nothing to assert beyond "raft
// needs a majority" — which is true of NATS and not a property of ours.
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
		pkgFix = &Fixture{Env: env, AWS: awsCli, Harness: h, Cluster: cluster}
	})
	if pkgFixErr != nil {
		t.Fatalf("kvquorum fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset")
	}
	if n := len(pkgFix.Cluster.Nodes); n < 3 {
		t.Skipf("quorum under node loss needs a surviving majority, have %d node(s)", n)
	}
	return pkgFix
}

// TestKVBucketsAreReplicatedAcrossTheCluster is read-only and runs first, so a
// cluster that is already wrong is reported as wrong rather than as a failure
// to survive a node loss.
func TestKVBucketsAreReplicatedAcrossTheCluster(t *testing.T) {
	runBucketsAreReplicated(t, requireFixture(t))
}

// TestKVReplicasCommandReportsHealthy proves the operator surface agrees with
// the cluster, on every node, and that its exit status can gate a deployment.
func TestKVReplicasCommandReportsHealthy(t *testing.T) {
	runReplicasCommand(t, requireFixture(t))
}

// TestKVBucketsSurviveANodeLoss is the assertion the suite exists for, and the
// only one that produces the fault rather than inspecting for it.
func TestKVBucketsSurviveANodeLoss(t *testing.T) {
	runBucketsSurviveANodeLoss(t, requireFixture(t))
}
