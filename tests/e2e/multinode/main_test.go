//go:build e2e

// Package multinode is the Go port of run-multinode-e2e.sh — the real
// 3-node E2E suite that drives the full EC2/VPC/NAT lifecycle against a
// pre-bootstrapped Spinifex cluster. Each phase from the bash driver runs
// as a top-level Test* in this package; ordering is not implied — every
// phase self-bootstraps its prerequisites via harness.Discover* /
// harness.Ensure* and the package-local need* helpers.
package multinode

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture, built lazily by requireMultiNodeFixture.
// A run with SPINIFEX_E2E unset stays cheap (no AWS dial, no temp dir).
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// TestMain owns process-level lifecycle for the singleton fixture.
func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil {
		if pkgFix.Harness != nil {
			pkgFix.Harness.Close()
		}
		if pkgFix.TmpDir != "" {
			_ = os.RemoveAll(pkgFix.TmpDir)
		}
	}
	os.Exit(code)
}

// requireMultiNodeFixture returns the package-scoped Fixture singleton, building
// it on first call. Skips if SPINIFEX_E2E is unset or mode is not multinode.
func requireMultiNodeFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeMultinode {
			return
		}
		cluster, cerr := harness.ClusterFromEnv()
		if cerr != nil {
			pkgFixErr = cerr
			return
		}

		// Pre-flight: catch a misconfigured image or broken peer networking
		// before any AWS API churn, with a clear message rather than letting
		// every later Test* fail on it independently.
		if err := checkKVMWritable(); err != nil {
			pkgFixErr = err
			return
		}
		if err := checkPeersReachable(cluster); err != nil {
			pkgFixErr = err
			return
		}

		awsCli := harness.NewAWSClient(t, env)
		h, herr := harness.NewProcessFixture(awsCli)
		if herr != nil {
			pkgFixErr = herr
			return
		}
		tmpDir, terr := os.MkdirTemp("", "multinode-pkgfix-*")
		if terr != nil {
			pkgFixErr = terr
			return
		}
		pkgFix = &Fixture{
			Env:     env,
			AWS:     awsCli,
			Harness: h,
			Cluster: cluster,
			TmpDir:  tmpDir,
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("multinode singleton fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("multinode singleton fixture unavailable (SPINIFEX_E2E unset or mode != multinode)")
	}
	return pkgFix
}

// checkKVMWritable fails fast if /dev/kvm is not writable on this node, which
// would otherwise surface confusingly deep inside the first instance launch.
func checkKVMWritable() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("preflight: /dev/kvm not writable: %w", err)
	}
	return f.Close()
}

// checkPeersReachable SSHes `hostname` to every remote cluster node (skipping
// node1, the runner itself — looping back over SSH tests nothing about peer
// reachability) so a broken peer network fails here instead of mid-suite.
func checkPeersReachable(cluster *harness.Cluster) error {
	ssh := harness.NewPeerSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range cluster.Nodes[1:] {
		if _, err := ssh.Run(ctx, n.Addr, "hostname"); err != nil {
			return fmt.Errorf("preflight: peer_ssh %s (%s): %w", n.Name, n.Addr, err)
		}
	}
	return nil
}

// Fixture carries the per-process state shared across every Test* in this package.
// Per-phase resource IDs are memoized on Harness via need* / harness.Ensure* helpers.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	Cluster *harness.Cluster // 3-node WAN-IP cluster from SPINIFEX_NODES.
	TmpDir  string           // package-scoped scratch dir; survives every Test* in the package.
}

// ArtifactDir returns a fresh per-test artifact directory scoped to t, matching
// the convention used by every other e2e package's Fixture (gpu, single,
// storagegrowth, iam). Call this with the CURRENT test's t at each diagnostic
// call site rather than caching the result: harness.ArtifactDir names the
// directory after t.Name() and registers a t.Cleanup that removes it when t
// passes, so a value computed once against whichever Test* happens to win the
// pkgFixOnce race would be pruned out from under every later test in the
// package. Resources that must outlive the whole package run (e.g. the shared
// SSH key pair pem) belong under Fixture.TmpDir instead.
func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}
