//go:build e2e

// Package storagegrowth drives guest workloads that make predastore's backing
// store grow faster than the data it holds, so an external step can measure the
// gap. Data should settle at ~1.5x its logical footprint — the RS 2+1 erasure
// floor (data=2 parity=1 yields three half-size shards, which is not 3x
// replication) — plus a small AES-GCM fragment-framing overhead. Anything above
// that, and especially anything that grows monotonically, is a leak.
//
// It exists to isolate the *unreferenced-live* leak, which only a real guest
// workload can produce: viperblock mints chunk objects under unique monotonic
// keys, so they are never overwritten. It simply stops referencing an old chunk
// and never issues a Delete. predastore is behaving correctly by its own lights
// — nobody told it the object is garbage — so it counts those bytes LIVE.
//
// That is a different fault from the *overwrite-orphan* leak, where a key is
// rewritten in place and the superseded extent is never tombstoned, so
// compaction can never find it. Keeping the two apart is the whole point, and
// it is why this package does not measure. The two leaks are distinguished
// precisely by predastore's own live-versus-dead split: unreferenced-live bytes
// inflate live totals, overwrite-orphan bytes inflate dead totals. A host-side
// `du` reports one number that mixes them, which cannot separate the target from
// the confound — and this workload drives both at once, because the drain each
// pass forces also rewrites the checkpoint map in place. So the backend is
// measured out of band by a tool that reads predastore's accounting directly.
// That tool does not exist in a standalone spinifex checkout, and depending on
// it here would break this suite's CI.
//
// The division of labour: this package drives the guest and reports what it did
// (see npassWorkload); the measurement step reads that record for the measured
// denominator and supplies the backend numbers. Run standalone, the workload
// still asserts the guest-side invariants it can see on its own.
//
// This is its own package rather than living in single/ or multinode/:
// single/ shares one singleton instance across the whole package that a
// multi-pass, multi-MiB workload would wreck for sibling tests, and
// multinode/'s fixture hard-skips outside ModeMultinode — meaningless on
// the single-node topology this fault was observed on. It is also
// deliberately excluded from the default e2e run and from
// docs/service-interfaces.yaml: like gpu/, select-suites never selects it
// automatically, so it is driven only by its own `make e2e-storagegrowth`
// target. Nothing under tests/e2e/harness/ is modified by this suite —
// that path is an infra glob, and editing it forces the full CI matrix on
// every change.
package storagegrowth

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
	pkgFixOnce    sync.Once
	pkgFix        *Fixture
	pkgFixErr     error
	pkgSkipReason string
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

// Fixture carries per-process state shared across storage-growth tests.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture // memoized Ensure* fixture; spans the whole process.
	TmpDir  string           // package-scoped scratch dir; survives every Test* in the package.

	// PoolMode mirrors single/'s detectPoolMode. external_mode="pool" is
	// required for this suite to run at all: EIP allocation is what makes
	// guest SSH reachable, and guest SSH is the only channel for measuring
	// the guest-side denominator. A nat-mode env has no EIP and no inbound
	// path, so GuestExec is unusable and the suite skips outright.
	PoolMode bool
}

// ArtifactDir returns the artifact directory for the *currently running*
// test. Derived per-call from the live t (not memoized on the singleton
// Fixture): the process fixture is built lazily during whichever test runs
// first, so a stored dir would freeze to that test's name.
func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requireStorageGrowthFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips the calling test when:
//   - SPINIFEX_E2E is unset
//   - SPINIFEX_MODE is not "single"
//   - external_mode is not "pool" (no EIP → no guest SSH → nothing to measure)
func requireStorageGrowthFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			pkgSkipReason = "storagegrowth suite requires SPINIFEX_MODE=single"
			return
		}
		// Guard against harness.NewAWSClient calling t.Fatal (which exits via
		// runtime.Goexit and corrupts the Once state for subsequent tests) when
		// no CA cert is resolvable. ResolveCACert uses the same candidate
		// paths NewAWSClient falls back to, so a failure here gives a clean
		// skip with an actionable message — but only when NewAWSClient would
		// actually take that path: SPINIFEX_AWS_INSECURE=1 makes it skip CA
		// resolution entirely (see harness/aws.go), which is how a
		// runner-resident scenario reaches a remote cluster that doesn't
		// have the spinifex cert on disk.
		if os.Getenv("SPINIFEX_AWS_INSECURE") != "1" {
			if _, err := harness.ResolveCACert(env); err != nil {
				pkgSkipReason = "no Spinifex CA cert found: " + err.Error() +
					" — provision a local node first (ansible-playbook ansible/playbooks/dev-reset.yml), " +
					"or target a remote cluster with SPINIFEX_AWS_INSECURE=1 (skips CA verification; see harness/aws.go)"
				return
			}
		}
		awsCli := harness.NewAWSClient(t, env)

		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		tmpDir, err := os.MkdirTemp("", "storagegrowth-pkgfix-*")
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
		t.Fatalf("storagegrowth fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		if pkgSkipReason != "" {
			t.Skip(pkgSkipReason)
		}
		t.Skip("SPINIFEX_E2E unset")
	}
	if !pkgFix.PoolMode {
		t.Skip("storagegrowth suite requires external_mode=pool — a nat env has no EIP and cannot reach guest SSH, the only channel for the guest-side denominator")
	}
	return pkgFix
}

// detectPoolMode reads external_mode from spinifex.toml. Defaults to false
// (dev_networking). Copied from single/'s helper rather than imported —
// tests/e2e/harness/** is deliberately never touched by this suite (see
// package doc), and this predicate is package-local everywhere it already
// exists.
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
