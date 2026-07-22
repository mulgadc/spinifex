//go:build e2e

// Package partialblock proves that concurrent sub-block writes to the same
// block survive a cold reread when they travel through NBD from a real guest.
//
// The claim is deliberately narrow. The cheap, deterministic gate for the
// underlying read-modify-write race is viperblock's own unit reproducer, which
// runs against a real predastore in a fraction of a second. This suite adds
// exactly one thing that unit test cannot: evidence that the race is reachable
// end to end, through the guest block layer and the NBD transport, rather than
// only through the engine's Go API.
//
// It is its own package rather than a case inside single/ because single/
// shares one memoized instance across the whole package, and this suite needs
// a raw, unformatted volume it can write with O_DIRECT and then delete. A
// filesystem is precisely what prevents the workload from reaching the code
// under test: it coalesces writes into aligned full-block requests, and the
// read-modify-write path only runs for a partial block.
package partialblock

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

// Fixture carries per-process state shared across this package's tests.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture
	TmpDir  string

	// PoolMode gates the suite: EIP allocation is what makes guest SSH
	// reachable (harness.InstancePublicSSHHost refuses to fall back to
	// qemu-hostfwd), and guest SSH is the only channel this suite has for
	// driving the write workload and reading its verdict back.
	PoolMode bool
}

func (f *Fixture) ArtifactDir(t *testing.T) string {
	t.Helper()
	return harness.ArtifactDir(t, f.Env)
}

// requirePartialBlockFixture returns the package-scoped Fixture singleton,
// building it on first call. Skips when SPINIFEX_E2E is unset, the mode is not
// single-node, or the env has no EIP pool.
func requirePartialBlockFixture(t *testing.T) *Fixture {
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
		tmpDir, err := os.MkdirTemp("", "partialblock-pkgfix-*")
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
		t.Fatalf("partialblock fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E unset or SPINIFEX_MODE != single")
	}
	if !pkgFix.PoolMode {
		t.Skip("partialblock suite requires external_mode=pool — a nat env has no EIP and cannot reach guest SSH, the only channel this suite has to drive and verify the write workload")
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
