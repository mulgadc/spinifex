//go:build e2e

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ArtifactDir returns the per-test artifact directory and registers a Cleanup
// that prunes it on test pass. Safe to call from subtests.
func ArtifactDir(t *testing.T, env *Env) string {
	t.Helper()
	safeName := strings.ReplaceAll(t.Name(), "/", "_")
	dir := filepath.Join(env.ArtifactDir, safeName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("artifact dir: %v", err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.RemoveAll(dir)
		}
	})
	return dir
}

// DumpFile writes content to <ArtifactDir>/<name>. Errors are logged but
// do not fail the test — artifacts are best-effort diagnostics.
func DumpFile(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Logf("dump %s: %v", path, err)
	}
}

// DumpCmd runs cmd and writes stdout+stderr to <ArtifactDir>/<name>.
// Best-effort — does not fail the test.
func DumpCmd(t *testing.T, dir, name string, cmd string, args ...string) {
	t.Helper()
	out, err := exec.Command(cmd, args...).CombinedOutput()
	header := fmt.Sprintf("$ %s %s\n", cmd, strings.Join(args, " "))
	if err != nil {
		header += fmt.Sprintf("(exit error: %v)\n", err)
	}
	DumpFile(t, dir, name, append([]byte(header), out...))
}

// OnFailure registers fn to run only if the test failed. Use to dump
// expensive state (journal logs, VM consoles, predastore listings).
func OnFailure(t *testing.T, fn func()) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			fn()
		}
	})
}
