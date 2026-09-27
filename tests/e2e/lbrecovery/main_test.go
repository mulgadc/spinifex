//go:build e2e

// Package lbrecovery proves what a customer sees when the node under a load
// balancer fails: the name keeps resolving, the address does not move, and
// traffic comes back on its own.
//
// It is its own suite for the reason instancerecovery is. It takes a node away,
// so anything sharing the environment would see the same outage and report a
// failure that says nothing about itself; and it needs its own VPC, its own
// public subnet and backends placed relative to the load balancer's node, which
// is a topology no shared fixture describes. A failure here also has to be
// readable as "the load balancer did not survive its host" rather than as one
// more red subtest in a suite about something else.
package lbrecovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// How long the ALB is given to reach active on a healthy cluster. A system
	// instance has to be launched, booted, and reported before anything here
	// starts.
	albActiveBudget = 5 * time.Minute

	// How long the backends are given to pass their health checks, on a 5s
	// interval with a healthy threshold of 2.
	targetsHealthyBudget = 3 * time.Minute

	// How long a survivor is given to claim and relaunch the ALB. The owner has
	// to be seen stale across two passes, the volume leases have to expire, and
	// then a guest has to boot — the same arithmetic as an ordinary instance
	// recovery, with HAProxy's own start on top.
	albRecoveryBudget = 10 * time.Minute

	// How long traffic is given to come back once a survivor is running the ALB.
	// The load balancer has to re-establish its own health checks against the
	// backends before it will forward anything.
	albServingBudget = 6 * time.Minute

	// How often the DNS watch samples every surviving node. The record is the
	// only thing a customer holds, so the sampling is fast enough that a gap
	// lasting one reconcile pass would be seen.
	dnsWatchInterval = 5 * time.Second
)

// errSkipDevNetworking marks an environment where the scenario has nothing to
// assert: a dev-networking cluster allocates no external addresses, so an
// internet-facing load balancer has no address to keep across a recovery.
var errSkipDevNetworking = errors.New("dev networking")

// devNetworking reports whether this cluster is configured for dev networking.
func devNetworking(env *harness.Env) bool {
	if env == nil || env.ConfigDir == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(env.ConfigDir, "spinifex.toml"))
	if err != nil {
		return false
	}
	return bytes.Contains(raw, []byte("dev_networking = true"))
}

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
// Three nodes is the floor, and for one more reason than instancerecovery's:
// recovery needs a survivor that is not the node that failed, and this scenario
// also needs a node for the backends that is not the load balancer's, so that
// taking the load balancer's node away does not take its backends with it.
func requireFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if devNetworking(env) {
			pkgFixErr = errSkipDevNetworking
			return
		}
		harness.RequireDNSEnabled(t, env)
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
	if errors.Is(pkgFixErr, errSkipDevNetworking) {
		t.Skip("dev_networking is on, so this cluster has no external address for a load balancer to keep")
	}
	if pkgFixErr != nil {
		t.Fatalf("lbrecovery fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset")
	}
	if n := len(pkgFix.Cluster.Nodes); n < 3 {
		t.Skipf("a load balancer needs a survivor to move to and a node for its backends, have %d", n)
	}
	return pkgFix
}

// TestALBSurvivesItsHostFailing is the whole suite. It is one test because it is
// one story, and splitting it would mean taking a node away twice to assert two
// halves of the same outage.
func TestALBSurvivesItsHostFailing(t *testing.T) {
	runALBSurvivesItsHostFailing(t, requireFixture(t))
}
