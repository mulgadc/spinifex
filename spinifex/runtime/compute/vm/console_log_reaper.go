package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

const (
	// consoleTrimAbove is the size past which a console log is cut back.
	consoleTrimAbove = 4 << 20
	// consoleKeepBytes is how much of the newest output survives a cut.
	consoleKeepBytes = 1 << 20
	// consoleRemoveAfter is how long a retired console file lingers so a late
	// GetConsoleOutput still sees it.
	consoleRemoveAfter = time.Hour
	// consoleIdleAfter is how long a live console file must sit unchanged, with
	// no QEMU on this node holding it open, before it is removed.
	consoleIdleAfter = time.Hour
)

// consoleTerminatedSuffix marks the console file of an instance that is gone
// for good. The log shipper only follows console-*.log, so a renamed file is
// no longer shipped, and the reaper removes it once it is old enough.
const consoleTerminatedSuffix = ".terminated"

// RetiredConsolePath is where the console file at path moves when its instance
// is terminated.
func RetiredConsolePath(path string) string { return path + consoleTerminatedSuffix }

// retireConsoleLog renames an ended instance's console file so the reaper can
// age it out. The file is final by now: QEMU is down or being force-finalized.
func retireConsoleLog(path string) {
	if path == "" {
		return
	}
	if err := os.Rename(path, RetiredConsolePath(path)); err != nil && !os.IsNotExist(err) {
		slog.Warn("vm: retire console log failed", "path", path, "err", err)
	}
}

// ConsoleLogReaper caps the serial console files under dir and removes the
// retired ones. A live console-<id>.log goes only after an idle hour with no
// QEMU on this node holding it open, decided from /proc alone and never from
// instance state.
type ConsoleLogReaper struct {
	dir      string
	procRoot string
	now      func() time.Time
}

var _ Reaper = (*ConsoleLogReaper)(nil)

// NewConsoleLogReaper builds a reaper over the console files in dir.
func NewConsoleLogReaper(dir string) *ConsoleLogReaper {
	return &ConsoleLogReaper{dir: dir, procRoot: qemuProcRoot, now: time.Now}
}

func (r *ConsoleLogReaper) Class() string      { return "console-log" }
func (r *ConsoleLogReaper) Scope() ReaperScope { return ScopeNodeLocal }

// NewLocalConsoleLogReaper builds the reaper over this node's runtime directory.
func (m *Manager) NewLocalConsoleLogReaper() *ConsoleLogReaper {
	return NewConsoleLogReaper(hostprocess.RuntimeDir())
}

// Sweep removes idle live console files nothing holds open, cuts oversized ones
// back to their newest consoleKeepBytes, and removes retired ones that have sat
// for over an hour.
func (r *ConsoleLogReaper) Sweep(context.Context) (int, error) {
	reaped := 0
	live, err := filepath.Glob(filepath.Join(r.dir, "console-*.log"))
	if err != nil {
		return 0, err
	}
	now := r.now()
	warned := false
	for _, p := range live {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) > consoleIdleAfter && r.removeIfUnheld(p, &warned) {
			reaped++
			continue
		}
		if info.Size() <= consoleTrimAbove {
			continue
		}
		if err := trimConsoleLog(p); err != nil {
			slog.Warn("vm/gc: trim console log failed", "path", p, "err", err)
			continue
		}
		reaped++
	}
	retired, err := filepath.Glob(filepath.Join(r.dir, "console-*.log"+consoleTerminatedSuffix))
	if err != nil {
		return reaped, err
	}
	for _, p := range retired {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= consoleRemoveAfter {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("vm/gc: remove retired console log failed", "path", p, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// removeIfUnheld removes path when no QEMU process on this node has it open. If
// the process table cannot be read in full it removes nothing, since a guest
// that is merely quiet must keep its log.
func (r *ConsoleLogReaper) removeIfUnheld(path string, warned *bool) bool {
	held, err := qemuOpenFiles(r.procRoot, r.dir)
	if err != nil {
		if !*warned {
			slog.Warn("vm/gc: idle console logs kept, cannot tell whether QEMU holds them", "err", err)
			*warned = true
		}
		return false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if held[path] {
		return false
	}
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("vm/gc: remove idle console log failed", "path", path, "err", err)
		}
		return false
	}
	return true
}

// qemuOpenFiles returns the files every QEMU process under procRoot has open.
// Any unreadable descriptor table is an error, never a partial answer. The scan
// is complete only while the daemon keeps CAP_SYS_ADMIN under ProtectProc=invisible,
// so an instance pidfile naming a live pid the scan missed is an error too.
func qemuOpenFiles(procRoot, runtimeDir string) (map[string]bool, error) {
	pids, err := scanLiveQEMUPIDs(procRoot)
	if err != nil {
		return nil, err
	}
	if err := checkPidFilesVisible(runtimeDir, pids); err != nil {
		return nil, err
	}
	held := make(map[string]bool)
	for _, pid := range pids {
		fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			held[target] = true
		}
	}
	return held, nil
}

// checkPidFilesVisible fails when an instance pidfile names a process that is
// alive but absent from seen. Signal 0 answers even when /proc hides the pid.
func checkPidFilesVisible(runtimeDir string, seen []int) error {
	visible := make(map[int]bool, len(seen))
	for _, pid := range seen {
		visible[pid] = true
	}
	for pid, id := range pidFileOwners(runtimeDir) {
		if !strings.HasPrefix(id, "i-") || visible[pid] {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			return fmt.Errorf("pidfile for %s names live pid %d that the process scan cannot see", id, pid)
		}
	}
	return nil
}

// trimConsoleLog rewrites the file in place as its newest consoleKeepBytes. The
// guest's writer keeps its descriptor (O_APPEND), so output written while the
// copy runs may be lost but the writer is never blocked.
func trimConsoleLog(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= consoleTrimAbove {
		return nil
	}
	tail := make([]byte, consoleKeepBytes)
	if _, err := f.ReadAt(tail, info.Size()-consoleKeepBytes); err != nil && err != io.EOF {
		return err
	}
	if _, err := f.WriteAt(tail, 0); err != nil {
		return err
	}
	return f.Truncate(consoleKeepBytes)
}
