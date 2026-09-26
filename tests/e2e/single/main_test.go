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
		fix.PublicPool, fix.PublicPoolSource = detectPublicPool(env)
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
	Env        *harness.Env
	AWS        *harness.AWSClient
	Harness    *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	TmpDir     string           // package-scoped scratch dir; survives every Test* in the package.
	PublicPool bool             // public addresses can be allocated; gates 8b / 8d

	// PublicPoolSource is the first non-transit pool's source ("static",
	// "dhcp", "oci"), empty when there is none. A stop returns an auto-assigned
	// address whatever the source, but only a pool we number ourselves can be
	// asserted to hand out a different one on the start that follows.
	PublicPoolSource string
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

// detectPublicPool reports whether this cluster can hand out public
// addresses, and where the first customer-facing pool gets them from. Both
// external_mode "pool" and external_mode "nat" with a non-transit pool
// configured can: routed NAT carries EIPs, so gating on the mode name alone
// skipped the egress and SMTP phases on a cluster that supports every call
// they make.
func detectPublicPool(env *harness.Env) (bool, string) {
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	f, err := os.Open(cfg)
	if err != nil {
		return false, ""
	}
	defer f.Close()

	mode, section, source := "", "", ""
	hasPool, inTransit := false, false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			// A new table means a new pool, and its source has not been read
			// yet — carrying the previous one over would mislabel it.
			inTransit = false
			continue
		}
		_, rhs, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val := strings.Trim(strings.TrimSpace(rhs), "\"'")
		switch {
		case section == "[network]" && strings.HasPrefix(line, "external_mode"):
			mode = val
		// The transit pool is routed NAT's own plumbing, not addresses a
		// customer can allocate, so it is not evidence of a public pool.
		case section == "[[network.external_pools]]" && strings.HasPrefix(line, "name"):
			inTransit = val == "nat-transit"
			hasPool = hasPool || !inTransit
		case section == "[[network.external_pools]]" && strings.HasPrefix(line, "source") && !inTransit && source == "":
			source = val
		}
	}
	// An omitted source is the static default, and only a real pool has one.
	if hasPool && source == "" {
		source = "static"
	}
	return mode == "pool" || (mode == "nat" && hasPool), source
}
