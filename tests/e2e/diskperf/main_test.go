//go:build e2e

// Package diskperf is the guest disk-performance gate. It launches an
// instance, attaches a dedicated blank data volume, drives a pinned fio
// profile inside the guest, and asserts two separable classes of property.
//
// Availability assertions are hard pass/fail with zero tolerance: the fio job
// completes inside its budget, a guest responsiveness probe on a second SSH
// channel answers throughout, and the host's own sshd answers throughout. A
// healthy system sits orders of magnitude away from these bounds and a broken
// one sits orders of magnitude past them, so they do not flap. They are the
// class that catches a volume workload taking its guest, its neighbours or its
// host down, and they are meaningful on the very first run because they are
// absolute rather than relative.
//
// Throughput and latency assertions are banded against a committed baseline:
// fail above a 25% regression, warn above 10%. Wide enough to survive
// bare-metal jitter, tight enough to catch a 2x. A baseline entry that is
// absent records and warns instead of failing, so the gate can run before
// hardware numbers exist rather than blocking on them.
//
// Three things about the workload are deliberate. The target is a raw
// unformatted device rather than a file on the boot disk, because a filesystem
// layers guest page cache, allocation and journal traffic over what is being
// measured. Each job gets its own freshly created volume, because the extent
// index grows monotonically and reusing a volume makes results order-dependent
// and drifting upward over time -- a gate that slowly normalises the
// regression it exists to catch. And the guest responsiveness probe reads the
// root volume, not the volume under test, so a passing probe is evidence of
// isolation between volumes rather than of the workload merely being slow.
//
// The suite is excluded from the default e2e run and selected only by its own
// make target, like gpu/ and storagegrowth/. Nothing under tests/e2e/harness/
// is modified by it: that path is an infra glob, and editing it forces the
// full CI matrix on every change.
package diskperf

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
			// A leaked resource fails the run: the suite may have passed, but it
			// left state on the node that the next run will trip over.
			if err := pkgFix.Harness.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
				code = 1
			}
		}
	}
	os.Exit(code)
}

// Fixture carries per-process state shared across disk-performance tests.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture

	// PoolMode reports external_mode="pool". Guest SSH is the only channel for
	// driving fio and for the responsiveness probe, and a nat-mode env has no
	// EIP and no inbound path, so the suite skips outright without it.
	PoolMode bool
}

// ArtifactDir returns the artifact directory for the currently running test.
// Derived per-call from the live t rather than memoized on the singleton, so
// it does not freeze to whichever test happened to build the fixture.
func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requireDiskPerfFixture returns the package-scoped Fixture singleton, building
// it on first call. Skips the calling test when SPINIFEX_E2E is unset, when the
// mode is not "single", or when external_mode is not "pool".
func requireDiskPerfFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			pkgSkipReason = "diskperf suite requires SPINIFEX_MODE=single"
			return
		}
		// Guard against NewAWSClient calling t.Fatal, which exits via
		// runtime.Goexit and corrupts the Once state for subsequent tests.
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
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{
			Env:      env,
			AWS:      awsCli,
			Harness:  h,
			PoolMode: detectPoolMode(env),
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("diskperf fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		if pkgSkipReason != "" {
			t.Skip(pkgSkipReason)
		}
		t.Skip("SPINIFEX_E2E unset")
	}
	if !pkgFix.PoolMode {
		t.Skip("diskperf suite requires external_mode=pool — a nat env has no EIP and cannot reach guest SSH, the only channel for driving fio")
	}
	return pkgFix
}

// detectPoolMode reads external_mode from spinifex.toml, defaulting to false.
// Copied from single/'s helper rather than imported: tests/e2e/harness/** is
// an infra glob this suite deliberately never touches.
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
		if _, rhs, ok := strings.Cut(line, "="); ok {
			return strings.Trim(strings.TrimSpace(rhs), "\"'") == "pool"
		}
	}
	return false
}
