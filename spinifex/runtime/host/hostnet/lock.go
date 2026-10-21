package hostnet

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// acquireLock takes the exclusive flock setup-ovn.sh also takes, waiting at
// most env.LockWait, and returns the function that releases it.
func acquireLock(env Env) (func(), error) {
	path := env.Paths.Lock
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil { //nolint:gosec // G301: the mode setup.sh's tmpfiles entry gives /run/spinifex, which the daemon and vpcd traverse to check the marker. Holds no secret material.
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := env.Now().Add(env.LockWait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !env.Now().Before(deadline) {
			holder := lockHolder(f, "/proc/locks")
			_ = f.Close()
			return nil, fmt.Errorf("lock %s still held after %s by %s; is setup-ovn.sh running?", path, env.LockWait, holder)
		}
		env.Sleep(pollInterval)
	}
}

// lockHolder names the process holding a flock on f, from procLocks, whose
// rows read "1: FLOCK ADVISORY WRITE <pid> <maj>:<min>:<inode> 0 EOF".
func lockHolder(f *os.File, procLocks string) string {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "an unknown process"
	}
	id := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino) //nolint:unconvert // Dev is not uint64 on every arch
	locks, err := os.Open(procLocks)
	if err != nil {
		return "an unknown process"
	}
	defer locks.Close()
	sc := bufio.NewScanner(locks)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[5] != id {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}
		return describePID(pid)
	}
	return "an unknown process"
}

func describePID(pid int) string {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(cmdline) == 0 {
		return "PID " + strconv.Itoa(pid)
	}
	args := strings.TrimRight(strings.ReplaceAll(string(cmdline), "\x00", " "), " ")
	return fmt.Sprintf("PID %d (%s)", pid, args)
}
