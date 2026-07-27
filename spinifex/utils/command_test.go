package utils

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandWithTimeoutKillsProcessGroup(t *testing.T) {
	output, err := RunCommandWithTimeout(100*time.Millisecond, "sh", "-c", "echo waiting-for-lock >&2; sleep 10 & echo child=$!; wait")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	for _, want := range []string{"timed out after 100ms", "waiting-for-lock", "child="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}

	childPID := childPIDFromOutput(t, output)
	deadline := time.Now().Add(time.Second)
	for processRunning(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processRunning(childPID) {
		t.Errorf("child process %d survived command timeout", childPID)
	}
}

func TestRunCommandWithTimeoutKillsDetachedOutputHolder(t *testing.T) {
	started := time.Now()
	output, err := RunCommandWithTimeout(5*time.Second, "sh", "-c", "sleep 10 & echo child=$!")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("error = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("detached output holder returned after %s", elapsed)
	}

	childPID := childPIDFromOutput(t, output)
	deadline := time.Now().Add(time.Second)
	for processRunning(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processRunning(childPID) {
		t.Errorf("detached child process %d survived command cleanup", childPID)
	}
}

func TestRunCommandWithTimeoutIncludesDiagnostics(t *testing.T) {
	_, err := RunCommandWithTimeout(time.Second, "sh", "-c", "echo resolver-broke >&2; exit 7")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	for _, want := range []string{"sh -c", "resolver-broke", "exit status 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func childPIDFromOutput(t *testing.T, output string) int {
	t.Helper()
	for field := range strings.FieldsSeq(output) {
		value, ok := strings.CutPrefix(field, "child=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("parse child PID %q: %v", value, err)
		}
		return pid
	}
	t.Fatalf("child PID missing from output %q", output)
	return 0
}

func processRunning(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		fields := strings.Fields(string(data))
		return len(fields) < 3 || fields[2] != "Z"
	}
	if os.IsNotExist(err) {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
