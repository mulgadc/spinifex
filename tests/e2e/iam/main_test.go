//go:build e2e

// Package iam is the standalone control-plane IAM + STS E2E suite, split out of
// the single-node suite so it builds and runs as its own iam.test binary. Every
// entry point is a top-level Test* in tests_test.go; the suite is zero-VM (no
// RunInstances, no volumes, no OVN/SSH) so it can run concurrently with the
// single suite on one provisioned node, driving only the awsgw IAM/STS surface.
package iam

import (
	"os"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture. Initialised lazily by the first call to
// requireIAMFixture. TestMain drains its cleanup chain after m.Run() so every
// resource ensured during the run is torn down at process exit, regardless of
// which Test* created it.
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// TestMain owns process-level lifecycle for the singleton fixture. The
// singleton itself is built lazily by requireIAMFixture so a test run with
// SPINIFEX_E2E unset stays cheap (no AWS dial, no temp dir).
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

// requireIAMFixture returns the package-scoped Fixture singleton, building it on
// first call. Skips the calling test if SPINIFEX_E2E is unset or the cluster
// mode is not single-node. Fails the test if init itself errors (e.g. AWS
// client construction). PoolMode is detected so the instance-identity tests can
// gate their probe-VPC public-IP path.
func requireIAMFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			return
		}
		awsCli := harness.NewAWSClient(t, env)
		h, herr := harness.NewProcessFixture(awsCli)
		if herr != nil {
			pkgFixErr = herr
			return
		}
		tmpDir, terr := os.MkdirTemp("", "iam-pkgfix-*")
		if terr != nil {
			pkgFixErr = terr
			return
		}
		pkgFix = &Fixture{
			Env:      env,
			AWS:      awsCli,
			Harness:  h,
			TmpDir:   tmpDir,
			PoolMode: detectPoolMode(env),
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("iam fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("iam fixture unavailable (SPINIFEX_E2E unset or mode != single)")
	}
	return pkgFix
}

// Fixture carries the per-process state shared across every Test* in this
// package. Only environment-level slots — every per-phase resource ID is
// memoized on Harness (harness.Fixture) or the package-local iamEnsure* helpers.
type Fixture struct {
	Env      *harness.Env
	AWS      *harness.AWSClient
	Harness  *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	TmpDir   string           // package-scoped scratch dir; survives every Test* in the package.
	PoolMode bool             // external IPAM in play; gates the IMDS probe-VPC public-IP path.
}

// ArtifactDir returns the artifact directory for the *currently running* test.
// It must be derived per-call from the live t (not memoized on the singleton
// Fixture): the process fixture is built lazily during whichever test runs
// first, so a stored dir would freeze to that test's name and every later test
// would write into a stale — and, once that test passes, pruned — directory.
func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}
