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

func KillProcess(pid int) error {
	process, err := os.FindProcess(pid)

	if err != nil {
		return err
	}

	err = process.Signal(syscall.SIGTERM) // graceful shutdown
	if err != nil {
		return err
	}

	checks := 0
	for {
		time.Sleep(1 * time.Second)
		process, err = os.FindProcess(pid)

		if err != nil {
			return err
		}

		err = process.Signal(syscall.Signal(0))

		if err != nil {
			break // Signal(0) error means process has terminated
		}

		checks++

		if checks > 120 {
			err = process.Kill() // SIGKILL after 120 s

			if err != nil {
				return err
			}

			break
		}
	}

	return nil //nolint:nilerr // Signal(0) error means process exited — that's success
}

// KillProcessGraceful sends SIGTERM and waits indefinitely for the process to
// exit on its own. Use this when the process must flush state before exiting
// (e.g., nbdkit before a snapshot) and a SIGKILL would corrupt that state.
func KillProcessGraceful(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if err = process.Signal(syscall.SIGTERM); err != nil {
		return err
	}

	for {
		time.Sleep(1 * time.Second)
		process, err = os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err = process.Signal(syscall.Signal(0)); err != nil {
			break // process exited
		}
	}

	return nil //nolint:nilerr // Signal(0) error means process exited — that's success
}
