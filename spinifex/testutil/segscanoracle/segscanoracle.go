// Package segscanoracle is a storage-state oracle that runs the mulga
// umbrella repo's scripts/segscan against a copy of a real predastore node
// data dir, reporting live / dead-tombstoned / dead-orphan bytes per the
// on-disk .seg segments. It is the segscan-side counterpart to
// spinifex/testutil/vbscan (see that package's comment for the chunk/config
// oracle) — together they let integration tests assert real persisted
// storage state instead of only "the handler did not error".
//
// scripts/segscan is `package main` in its own Go module
// (github.com/mulgadc/mulga/scripts/segscan) and cannot be imported, so this
// package builds and execs it rather than calling into it directly. That
// module lives in the mulga umbrella repo, not in spinifex, so it is only
// available where a caller has arranged for it to be checked out alongside
// spinifex (see Locate) — tests using this oracle must tolerate it being
// absent and skip rather than fail.
package segscanoracle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/fixtures/scratch"
	"github.com/stretchr/testify/require"
)

// segscanEnvVar lets a caller point directly at scripts/segscan's source
// dir, overriding the sibling-checkout guesses in Locate. CI sets this
// explicitly rather than relying on the guess (see the segscan-oracle
// workflow).
const segscanEnvVar = "SEGSCAN_DIR"

// buildDirPrefix names the shared build's temp dir so a later run's
// scratch.SweepAbandoned can reclaim one stranded by a killed process,
// mirroring tests/fixtures/predastore's fixtureDirPrefix.
const buildDirPrefix = "segscanoracle-bin-"

// Locate finds scripts/segscan's module source directory. SEGSCAN_DIR
// overrides when set; otherwise it tries the two layouts spinifex is
// checked out under: a plain monorepo clone (mulga/spinifex with sibling
// mulga/scripts/segscan) and CI's sparse-checkout sibling (workspace/spinifex
// with workspace/mulga/scripts/segscan). Returns an error — never panics or
// fails a test — so callers can decide to skip.
func Locate() (string, error) {
	if dir := os.Getenv(segscanEnvVar); dir != "" {
		if hasGoMod(dir) {
			return dir, nil
		}
		return "", fmt.Errorf("%s=%s has no go.mod", segscanEnvVar, dir)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("segscanoracle: could not resolve source location for module-relative lookup")
	}
	// thisFile is <spinifex-root>/spinifex/testutil/segscanoracle/segscanoracle.go.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))

	candidates := []string{
		filepath.Join(root, "..", "scripts", "segscan"),          // monorepo: mulga/spinifex + mulga/scripts/segscan
		filepath.Join(root, "..", "mulga", "scripts", "segscan"), // CI: workspace/spinifex + workspace/mulga/scripts/segscan
	}
	for _, c := range candidates {
		if hasGoMod(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("scripts/segscan source not found (tried %v); set %s or check out the mulga umbrella repo alongside spinifex", candidates, segscanEnvVar)
}

func hasGoMod(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil && !info.IsDir()
}

// Totals mirrors the subset of scripts/segscan's --json "totals" object this
// oracle asserts against. Field names/tags are load-bearing: if segscan ever
// renames or restructures these, json.Unmarshal silently zeroes the
// corresponding field here rather than erroring, and the test consuming this
// package's Report is what turns that into a loud, failing assertion (e.g. an
// unexpectedly-zero LivePhysical after a real write) — see this repo's
// segscan_oracle_test.go for that check.
type Totals struct {
	LiveLogical    int64 `json:"liveLogical"`
	LivePhysical   int64 `json:"livePhysical"`
	DeadTombstoned int64 `json:"deadTombstonedReclaimable"`
	DeadOrphan     int64 `json:"deadOrphanUnreclaimable"`
}

// Report mirrors the store-wide subset of scripts/segscan's --json output
// this oracle needs. Per-segment detail is intentionally omitted: nothing
// under tests/ currently needs it, and declaring it would just be more
// surface for the JSON shape to drift against unused.
type Report struct {
	Totals Totals `json:"totals"`
}

// buildResult caches build's outcome for the life of the test binary. A
// struct field, rather than a package-level `var ... error`, sidesteps
// golangci's errname check (which reads any package-level error variable as
// a mis-named sentinel error) — this is a memoized result, not a sentinel.
type buildResult struct {
	bin string
	err error
}

var (
	buildMu    sync.Mutex
	buildDone  bool
	buildState buildResult
)

// build compiles scripts/segscan once per test binary (subsequent calls
// reuse the same binary) and returns its path. Like testpredastore's shared
// daemon, the build output must outlive any individual test, so it lives
// under a swept os.MkdirTemp dir rather than t.TempDir().
func build(t *testing.T, srcDir string) (string, error) {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildDone {
		return buildState.bin, buildState.err
	}
	buildDone = true

	scratch.SweepAbandoned(os.TempDir(), buildDirPrefix, scratch.DefaultMaxAge)

	binDir, err := os.MkdirTemp("", buildDirPrefix+"*") //nolint:usetesting // shared across every test in this binary, outlives any one of them
	if err != nil {
		buildState.err = fmt.Errorf("create build dir: %w", err)
		return "", buildState.err
	}
	bin := filepath.Join(binDir, "segscan")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = srcDir
	// GOWORK=off: scripts/segscan is its own module with its own go.mod/go.sum.
	// An ancestor directory of srcDir may have an unrelated go.work (the mulga
	// umbrella repo has one, and CI's checkout layout can produce another one
	// that doesn't `use` scripts/segscan at all) — force single-module mode so
	// this build never depends on which, if any, go.work Go finds by walking
	// up from srcDir.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		buildState.err = fmt.Errorf("go build %s: %w\n%s", srcDir, err, stderr.String())
		return "", buildState.err
	}

	buildState.bin = bin
	return buildState.bin, nil
}

// CopyNodeDir copies srcDir — a live predastore node data dir (db/, *.seg,
// *.idx, state.json) — into a fresh t.TempDir() and returns the copy's path.
// segscan opens the Badger index read-write and truncates its value log on
// open (see scripts/segscan's own --help text), so it must never run
// directly against a live store's data dir; this is the only sanctioned way
// to get a dir Run can be pointed at.
func CopyNodeDir(t *testing.T, srcDir string) string {
	t.Helper()
	dst := t.TempDir()

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755) //nolint:gosec // ephemeral test-only copy
		}
		return copyFile(path, target)
	})
	require.NoError(t, err, "copy node dir %s", srcDir)
	return dst
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// Run builds scripts/segscan (once per test binary) and execs it against
// copiedDataDir with --json, returning the decoded report. copiedDataDir
// MUST already be a copy — see CopyNodeDir — never a live node's data dir.
//
// If scripts/segscan's source isn't available (Locate fails — the normal
// case outside the dedicated segscan-oracle CI job, which is the only place
// the mulga umbrella repo is checked out alongside spinifex), Run skips the
// test rather than failing it. A build or exec failure once the source is
// found is a real problem and fails the test.
func Run(t *testing.T, copiedDataDir string) *Report {
	t.Helper()

	srcDir, err := Locate()
	if err != nil {
		t.Skipf("segscanoracle: %v", err)
	}

	bin, err := build(t, srcDir)
	if err != nil {
		t.Fatalf("segscanoracle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "--dir", copiedDataDir, "--json")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("segscanoracle: %s --dir %s --json: %v\nstderr:\n%s", bin, copiedDataDir, err, stderr.String())
	}

	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("segscanoracle: decode segscan JSON: %v\noutput:\n%s", err, stdout.String())
	}
	return &rep
}
