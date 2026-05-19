package utils

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func ReadPidFile(name string) (int, error) {
	pidPath := pidPath()

	pidFile, err := os.ReadFile(filepath.Join(pidPath, fmt.Sprintf("%s.pid", name)))

	if err != nil {
		return 0, err
	}

	// Strip whitespace and /r or /n
	pidFile = bytes.TrimSpace(pidFile)

	return strconv.Atoi(string(pidFile))
}

func GeneratePidFile(name string) (string, error) {
	if name == "" {
		return "", errors.New("name is required")
	}

	pidPath := pidPath()

	if pidPath == "" {
		return "", errors.New("pid path is empty")
	}

	return filepath.Join(pidPath, fmt.Sprintf("%s.pid", name)), nil
}

func WritePidFile(name string, pid int) error {
	// Write PID to file, check XDG, otherwise user home directory ~/spinifex/
	pidFilename, err := GeneratePidFile(name)

	if err != nil {
		return err
	}

	pidFile, err := os.Create(pidFilename)

	if err != nil {
		return err
	}

	defer pidFile.Close()
	_, err = fmt.Fprintf(pidFile, "%d", pid)
	if err != nil {
		return err
	}

	return nil
}

// WritePidFileTo writes a PID file to a specific directory. If dir is empty,
// falls back to the default pidPath(). Used by services that know their own
// data directory (e.g. predastore's BasePath) to avoid PID file collisions
// when multiple nodes run on the same host.
func WritePidFileTo(dir string, name string, pid int) error {
	if dir == "" {
		return WritePidFile(name, pid)
	}

	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create pid directory %s: %w", dir, err)
	}

	pidFilename := filepath.Join(dir, fmt.Sprintf("%s.pid", name))

	pidFile, err := os.Create(pidFilename)
	if err != nil {
		return err
	}

	defer pidFile.Close()
	_, err = fmt.Fprintf(pidFile, "%d", pid)
	return err
}

// ReadPidFileFrom reads a PID from a file in a specific directory. If dir is
// empty, falls back to the default pidPath().
func ReadPidFileFrom(dir string, name string) (int, error) {
	if dir == "" {
		return ReadPidFile(name)
	}

	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s.pid", name)))
	if err != nil {
		return 0, err
	}

	data = bytes.TrimSpace(data)
	return strconv.Atoi(string(data))
}

// RemovePidFileAt removes a PID file from a specific directory. If dir is
// empty, falls back to the default pidPath().
func RemovePidFileAt(dir string, name string) error {
	if dir == "" {
		return RemovePidFile(name)
	}
	return os.Remove(filepath.Join(dir, fmt.Sprintf("%s.pid", name)))
}

// ServiceStatus returns a human-readable status string for a service by
// checking its PID file. If dir is empty, the default pidPath() is used.
func ServiceStatus(dir, name string) (string, error) {
	pid, err := ReadPidFileFrom(dir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return "stopped", nil
		}
		return "", fmt.Errorf("read pid file: %w", err)
	}
	return fmt.Sprintf("running (pid: %d)", pid), nil
}

// StopProcessAt stops a process using a PID file in a specific directory.
// If dir is empty, falls back to the default pidPath(). The PID file is
// always removed, even if the process is already dead, to prevent stale
// PID files from accumulating across restarts.
func StopProcessAt(dir string, name string) error {
	pid, err := ReadPidFileFrom(dir, name)
	if err != nil {
		return err
	}

	killErr := KillProcess(pid)

	// Always remove the PID file to avoid stale entries. If the process is
	// already dead, the PID file is stale and must be cleaned up.
	if removeErr := RemovePidFileAt(dir, name); removeErr != nil && killErr == nil {
		return removeErr
	}

	return killErr
}

func RemovePidFile(serviceName string) error {
	pidPath := pidPath()

	err := os.Remove(filepath.Join(pidPath, fmt.Sprintf("%s.pid", serviceName)))
	if err != nil {
		return err
	}

	return nil
}

// RuntimeDir returns the runtime directory used for PID files, sockets, and logs.
func RuntimeDir() string {
	return pidPath()
}

func pidPath() string {
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		return os.Getenv("XDG_RUNTIME_DIR")
	}
	if dirExists(fmt.Sprintf("%s/%s", os.Getenv("HOME"), "spinifex")) {
		return filepath.Join(os.Getenv("HOME"), "spinifex")
	}
	return os.TempDir()
}

// WaitForProcessExit polls until the given PID is no longer alive or the
// timeout expires. Unlike WaitForPidFileRemoval, this checks the process
// itself via kill(pid, 0), so it works after SIGKILL where the target
// cannot clean up its own PID file.
func WaitForProcessExit(pid int, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			return fmt.Errorf("timeout waiting for process %d to exit", pid)
		case <-ticker.C:
			proc, err := os.FindProcess(pid)
			if err != nil {
				return nil // process gone
			}
			if proc.Signal(syscall.Signal(0)) != nil {
				return nil // process no longer alive
			}
		}
	}
}

// WaitForPidFile polls until QEMU writes its pidfile or the timeout expires.
// Direct-boot microvms need ~50ms in the happy path, but under post-reboot
// recovery load (multiple VMs launching, nbdkit binding sockets, fwcfg blobs
// being written) the kernel may not schedule QEMU's pidfile write within a
// single settle window. A short blocking poll keeps the fast-start latency
// intact while avoiding premature launch-failure cascades that tear down the
// tap before QEMU finishes opening it.
func WaitForPidFile(instanceID string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		pid, err := ReadPidFile(instanceID)
		if err == nil {
			return pid, nil
		}
		if time.Now().After(deadline) {
			return 0, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// WaitForUnixSocket polls until a Unix socket exists at path or the timeout
// expires. QEMU binds its QMP socket after writing the pidfile, so callers
// that gate on WaitForPidFile can still race the socket bind under recovery
// load. Existence is checked via os.Stat rather than a dial probe so we don't
// consume the listener's accept queue before the real client connects.
func WaitForUnixSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for unix socket %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func WaitForPidFileRemoval(instanceID string, timeout time.Duration) error {
	timeoutCh := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCh:
			return fmt.Errorf("timeout waiting for PID file to be removed for instance %s", instanceID)
		case <-ticker.C:
			_, err := ReadPidFile(instanceID)
			if err != nil {
				// PID file no longer exists
				return nil
			}
		}
	}
}
