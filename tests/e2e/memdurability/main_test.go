//go:build e2e

// Package memdurability drives the decisive repro for a known-open
// data-loss bug: a guest sized close to its write working set, N large
// writes issued in parallel, a global sync, a cold page-cache drop, and a
// per-stream byte-equality check on reread. That intersection -- memory
// pressure, write concurrency, cold reread -- is where writes acked by
// viperblock have been observed to read back zero-filled.
//
// This is deliberately its own package rather than living in single/ or
// multinode/, for the same two reasons tests/e2e/storagegrowth/main_test.go
// gives for its own separation:
//
//   - single/ shares one memoized singleton instance
//     (harness.EnsureInstance) across the whole package. This suite needs a
//     guest booted at a *specific* memory ceiling close to its own write
//     working set -- sharing single/'s singleton would either undersize
//     every other single/ test sharing it, or, if sized for them instead,
//     fail to reproduce the memory-pressure condition this suite exists to
//     exercise.
//   - multinode/'s fixture hard-skips outside ModeMultinode, which is
//     irrelevant to a single-guest memory-pressure repro.
//
// It is also deliberately excluded from the default e2e run (the SUITES this
// package registers under is never part of .github/workflows/e2e.yml's
// default list) and from docs/service-interfaces.yaml, for a reason
// storagegrowth does NOT share: the data-loss bug this package's test
// encodes is still open. Until it is fixed, this test is EXPECTED to fail
// -- that is the fail-first evidence the regression test exists to
// demonstrate -- and a test that is expected to fail must never sit in a
// path that gates merges. It is still discoverable and runnable, not a
// silent skip: it is wired into workflow_dispatch as a non-default,
// explicitly-selected suite (see .github/workflows/e2e.yml and
// tests/e2e/Makefile's e2e-memdurability target).
package memdurability

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
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

// Fixture carries per-process state shared across this package's tests
// (currently one). Mirrors storagegrowth's Fixture shape.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture
	TmpDir  string

	// PoolMode gates the suite exactly as it gates storagegrowth: EIP
	// allocation is what makes guest SSH reachable
	// (harness.InstancePublicSSHHost refuses to fall back to qemu-hostfwd),
	// and guest SSH is the only channel this suite has for driving the
	// write workload and reading the cold-reread checksums back.
	PoolMode bool
}

func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requireMemDurabilityFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips when SPINIFEX_E2E is unset, the mode
// isn't single-node, or the env has no EIP pool.
func requireMemDurabilityFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			pkgFixErr = nil
			return
		}
		awsCli := harness.NewAWSClient(t, env)
		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		tmpDir, err := os.MkdirTemp("", "memdurability-pkgfix-*")
		if err != nil {
			pkgFixErr = err
			return
		}
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{
			Env:      env,
			AWS:      awsCli,
			Harness:  h,
			TmpDir:   tmpDir,
			PoolMode: detectPoolMode(env),
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("memdurability fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset or SPINIFEX_MODE != single")
	}
	if !pkgFix.PoolMode {
		t.Skip("memdurability suite requires external_mode=pool — a nat env has no EIP and cannot reach guest SSH, the only channel this suite has to drive and verify the write workload")
	}
	return pkgFix
}

// detectPoolMode reads external_mode from spinifex.toml. Copied rather than
// imported for the same reason storagegrowth copies it: harness/** is
// deliberately never touched by these standalone-workload packages, and the
// predicate is small and package-local everywhere it already exists.
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
		if !inNetwork || !strings.HasPrefix(line, "external_mode") {
			continue
		}
		// external_mode = "pool" — quoted value; only "pool" enables this suite.
		if _, rhs, ok := strings.Cut(line, "="); ok {
			val := strings.Trim(strings.TrimSpace(rhs), "\"'")
			return val == "pool"
		}
	}
	return false
}
