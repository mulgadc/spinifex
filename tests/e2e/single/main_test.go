//go:build e2e

// Package single is the Go port of run-e2e.sh — the single-node E2E suite
// that drives the full EC2/IAM lifecycle against a locally-bootstrapped
// Spinifex cluster. Each phase from the bash driver runs as a top-level
// Test* in this package; ordering is not implied — every phase
// self-bootstraps its prerequisites via harness.Discover* /
// harness.Ensure* (and the package-local need* / iamEnsure* helpers).
package single

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture. Initialised lazily by the first call
// to requireSingleNodeFixture. TestMain drains its cleanup chain after
// m.Run() so every resource ensured during the run is torn down at process
// exit, regardless of which Test* created it.
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// TestMain owns process-level lifecycle for the singleton fixture. The
// singleton itself is built lazily by requireSingleNodeFixture so a test
// run with SPINIFEX_E2E unset stays cheap (no AWS dial, no temp dir).
func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil {
		if pkgFix.Harness != nil {
			// A leaked resource fails the run: the suite may have passed, but it
			// left state on the node that the next run will trip over.
			if err := pkgFix.Harness.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
				code = 1
			}
		}
		if pkgFix.TmpDir != "" {
			_ = os.RemoveAll(pkgFix.TmpDir)
		}
	}
	os.Exit(code)
}

// requireSingleNodeFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips the calling test if SPINIFEX_E2E is
// unset or the cluster mode is not single-node. Fails the test if init
// itself errors (e.g. AWS client construction).
func requireSingleNodeFixture(t *testing.T) *Fixture {
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
		tmpDir, terr := os.MkdirTemp("", "single-pkgfix-*")
		if terr != nil {
			pkgFixErr = terr
			return
		}
		fix := &Fixture{
			Env:     env,
			AWS:     awsCli,
			Harness: h,
			TmpDir:  tmpDir,
		}
		fix.PoolMode = detectPoolMode(env)
		// admin init leaves the default SG closed (AWS parity); the e2e suite
		// drives SSH + ICMP probes from the test runner's external IP, so open
		// both on the default VPC's default SG once per process. Idempotent —
		// mirrors run-e2e.sh Phase 5.
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = fix
	})
	if pkgFixErr != nil {
		t.Fatalf("singleton fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("singleton fixture unavailable (SPINIFEX_E2E unset or mode != single)")
	}
	return pkgFix
}

// Fixture carries the per-process state shared across every Test* in this
// package. Only environment-level slots — every per-phase resource ID is
// memoized on Harness (harness.Fixture) and surfaced via the package-local
// need* helpers / harness.Ensure* + harness.Discover*.
type Fixture struct {
	Env      *harness.Env
	AWS      *harness.AWSClient
	Harness  *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	TmpDir   string           // package-scoped scratch dir; survives every Test* in the package.
	PoolMode bool             // gates 8b / 8d
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

// detectPoolMode reads external_mode from spinifex.toml. Defaults to false
// (dev_networking) which is the single-node CI fixture. Only external_mode
// "pool" enables Phase 8b/8d — nat clusters have no EIP/NAT-GW support, so
// AllocateAddress returns UnsupportedOperation and those phases must skip.
func detectPoolMode(env *harness.Env) bool {
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	f, err := os.Open(cfg)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inNetwork := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inNetwork = line == "[network]"
			continue
		}
		if !inNetwork {
			continue
		}
		if !strings.HasPrefix(line, "external_mode") {
			continue
		}
		// external_mode = "pool" — quoted value; only "pool" gates 8b/8d.
		if _, rhs, ok := strings.Cut(line, "="); ok {
			val := strings.Trim(strings.TrimSpace(rhs), "\"'")
			return val == "pool"
		}
	}
	return false
}
