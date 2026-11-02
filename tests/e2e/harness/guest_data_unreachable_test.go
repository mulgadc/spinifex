//go:build e2e

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuestUnreachableSeparatesSilenceFromAFailedCommand(t *testing.T) {
	exitWith := func(code int) error { return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run() }
	_, deadline := GuestExecTimeout(SSHTarget{}, "", 0)

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ssh could not connect", exitWith(255), true},
		{"remote command exited 1", exitWith(1), false},
		{"remote command exited 11", exitWith(11), false},
		{"command outlived its timeout", deadline, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GuestUnreachable(tc.err); got != tc.want {
				t.Fatalf("GuestUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// fakeSSH puts an ssh on PATH that exits 255 for its first `failures` calls and
// then runs `final`, counting every call in the returned file.
func fakeSSH(t *testing.T, failures int, final string) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := fmt.Sprintf("#!/bin/sh\nn=$(cat %[1]s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %[1]s\n"+
		"if [ $n -le %[2]d ]; then echo 'ssh: connect to host: Connection timed out' >&2; exit 255; fi\n%[3]s\n",
		counter, failures, final)
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func calls(t *testing.T, counter string) string {
	t.Helper()
	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// A guest that misses a few probes and then answers is read, not reported dead.
func TestGuestExecRetryingWaitsOutAGuestThatComesBack(t *testing.T) {
	counter := fakeSSH(t, 2, "echo rw-mounts")
	out, err := GuestExecRetrying(SSHTarget{}, "cat /proc/mounts", time.Second, 5*time.Second, 10*time.Millisecond)
	if err != nil || strings.TrimSpace(out) != "rw-mounts" {
		t.Fatalf("GuestExecRetrying = %q, %v; want the third attempt's answer", out, err)
	}
	if got := calls(t, counter); got != "3" {
		t.Errorf("ran ssh %s times, want 3", got)
	}
}

// A command that ran and failed is the guest's answer and is never retried.
func TestGuestExecRetryingReturnsAFailedCommandAtOnce(t *testing.T) {
	counter := fakeSSH(t, 0, "exit 1")
	if _, err := GuestExecRetrying(SSHTarget{}, "false", time.Second, 5*time.Second, 10*time.Millisecond); err == nil {
		t.Fatal("a failed command came back as success")
	}
	if got := calls(t, counter); got != "1" {
		t.Errorf("ran ssh %s times for a command that failed in the guest, want 1", got)
	}
}

// A guest that never answers is given up on once the bound has passed.
func TestGuestExecRetryingStopsAtItsBound(t *testing.T) {
	fakeSSH(t, 1<<30, "")
	start := time.Now()
	_, err := GuestExecRetrying(SSHTarget{}, "true", time.Second, 200*time.Millisecond, 20*time.Millisecond)
	if !GuestUnreachable(err) {
		t.Fatalf("an unreachable guest came back as %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("gave up after %s, well past its 200ms bound", took)
	}
}
