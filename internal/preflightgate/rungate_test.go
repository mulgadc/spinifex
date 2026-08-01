// Package preflightgate tests the preflight gate wrapper. It exists as a Go
// test so `go test ./...` exercises it; the repo's shell suites are not yet
// wired into CI.
package preflightgate

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath resolves scripts/run-gate.sh relative to this test file, so the
// test does not depend on the working directory.
func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "run-gate.sh")
}

func runGate(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(scriptPath(t), args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("running run-gate.sh: %v", err)
	return "", -1
}

func TestRunGate_CleanRunPasses(t *testing.T) {
	out, code := runGate(t, "govulncheck", "echo", "No vulnerabilities found.")
	if code != 0 {
		t.Fatalf("expected exit 0 for a clean run, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "No vulnerabilities found.") {
		t.Errorf("tool output should be forwarded, got:\n%s", out)
	}
}

func TestRunGate_NonZeroExitFails(t *testing.T) {
	out, code := runGate(t, "lint", "sh", "-c", "echo 3 issues.; exit 1")
	if code == 0 {
		t.Fatalf("expected failure for a non-zero tool exit, got 0\n%s", out)
	}
	if !strings.Contains(out, "lint FAILED") {
		t.Errorf("failure should name the gate, got:\n%s", out)
	}
}

// The defect this guards: a signal-killed scanner exiting 0 was reported clean,
// so preflight passed without a vulnerability scan having run.
func TestRunGate_SignalKilledWithZeroExitFails(t *testing.T) {
	for _, sig := range []string{"killed", "terminated", "aborted"} {
		t.Run(sig, func(t *testing.T) {
			out, code := runGate(t, "govulncheck", "sh", "-c",
				"echo 'go tool govulncheck: signal: "+sig+"'; exit 0")
			if code == 0 {
				t.Fatalf("expected failure when the tool died by signal, got 0\n%s", out)
			}
			if !strings.Contains(out, "did NOT complete") {
				t.Errorf("failure should say the gate did not complete, got:\n%s", out)
			}
		})
	}
}

// "signal" appearing in ordinary findings must not fail an otherwise clean run.
func TestRunGate_SignalWordInOutputDoesNotFail(t *testing.T) {
	out, code := runGate(t, "lint", "echo", "checked signal handling in 12 files")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", code, out)
	}
}

func TestRunGate_RequiresLabelAndCommand(t *testing.T) {
	out, code := runGate(t, "govulncheck")
	if code != 2 {
		t.Fatalf("expected usage exit 2, got %d\n%s", code, out)
	}
}
