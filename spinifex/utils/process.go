package utils

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// SetOOMScore sets the OOM score adjustment for a process.
// Score range: -1000 (never kill) to 1000 (always kill first).
// Linux-only; returns an error on non-Linux systems.
func SetOOMScore(pid int, score int) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("OOM score adjustment is only supported on Linux")
	}
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	return os.WriteFile(path, []byte(strconv.Itoa(score)), 0600)
}

func StopProcess(serviceName string) error {
	pid, err := ReadPidFile(serviceName)
	if err != nil {
		return err
	}

	err = KillProcess(pid)
	if err != nil {
		return err
	}

	err = RemovePidFile(serviceName)
	if err != nil {
		return err
	}

	return nil
}

// ProcessAlive reports whether the process is still running, via a
// signal-0 liveness probe. A PID file going missing does NOT imply the
// process exited, so callers that must reap a process check this directly.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// ForceKillProcess SIGKILLs a process immediately and waits up to timeout for
// it to exit. Used to reap a known-orphan (e.g. a QEMU for an
// already-terminated instance) where the graceful SIGTERM grace period is not
// warranted. SIGKILL cannot be caught, so the process never removes its own PID
// file — callers should RemovePidFile after this returns.
func ForceKillProcess(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	return WaitForProcessExit(pid, timeout)
}

// killProcessPollInterval and killProcessGracePeriod drive KillProcess's
// liveness poll: check every pollInterval instead of blocking for the full
// gracePeriod, so a process that dies quickly is detected almost immediately.
const (
	killProcessPollInterval = 50 * time.Millisecond
	killProcessGracePeriod  = 120 * time.Second
)

func KillProcess(pid int) error {
	return killProcessWithTiming(pid, killProcessPollInterval, killProcessGracePeriod)
}

// killProcessWithTiming implements KillProcess with an injectable poll
// interval and grace period so tests can exercise the SIGKILL escalation
// path without waiting out the full production grace period.
func killProcessWithTiming(pid int, pollInterval, gracePeriod time.Duration) error {
	process, err := os.FindProcess(pid)

	if err != nil {
		return err
	}

	err = process.Signal(syscall.SIGTERM) // graceful shutdown
	if err != nil {
		return err
	}

	deadline := time.Now().Add(gracePeriod)
	for {
		if !ProcessAlive(pid) {
			return nil // process has terminated
		}

		if time.Now().After(deadline) {
			process, err = os.FindProcess(pid)
			if err != nil {
				return err
			}

			if err := process.Kill(); err != nil { // SIGKILL after grace period
				return err
			}

			return nil
		}

		time.Sleep(pollInterval)
	}
}
