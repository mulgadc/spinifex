//go:build e2e

// Package multinode is the Go port of run-multinode-e2e.sh — the real
// 3-node E2E suite that drives the full EC2/VPC/NAT lifecycle against a
// pre-bootstrapped Spinifex cluster. Each phase from the bash driver runs
// as a top-level Test* in this package; ordering is not implied — every
// phase self-bootstraps its prerequisites via harness.Discover* /
// harness.Ensure* and the package-local need* helpers.
package multinode

import (
	"os"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture. Mirrors single/main_test.go. Built
// lazily by the first call to requireMultiNodeFixture so a test run with
// SPINIFEX_E2E unset stays cheap (no AWS dial, no temp dir).
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// TestMain owns process-level lifecycle for the singleton fixture.
// harness.Fixture.Close drains every cleanup at process exit, regardless
// of which Test* created it.
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

// requireMultiNodeFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips the calling test if SPINIFEX_E2E is
// unset or the cluster mode is not multinode. Fails the test if init
// itself errors (AWS client construction, cluster build).
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
			Env:       env,
			AWS:       awsCli,
			Harness:   h,
			Cluster:   cluster,
			Artifacts: harness.ArtifactDir(t, env),
			TmpDir:    tmpDir,
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

// Fixture carries the per-process state shared across every Test* in this
// package. Only environment-level slots — every per-phase resource ID is
// memoized on Harness (harness.Fixture) and surfaced via the package-local
// need* helpers / harness.Ensure* + harness.Discover*.
type Fixture struct {
	Env       *harness.Env
	AWS       *harness.AWSClient
	Harness   *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	Cluster   *harness.Cluster // 3-node WAN-IP cluster from SPINIFEX_NODES.
	Artifacts string
	TmpDir    string // package-scoped scratch dir; survives every Test* in the package.
}
