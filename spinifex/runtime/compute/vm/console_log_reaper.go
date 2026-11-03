package vm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// retired ones. It never removes a live console-<id>.log, whatever its age, so
// a stopped instance keeps its console without anyone reading instance state.
type ConsoleLogReaper struct {
	dir string
	now func() time.Time
}

var _ Reaper = (*ConsoleLogReaper)(nil)

// NewConsoleLogReaper builds a reaper over the console files in dir.
func NewConsoleLogReaper(dir string) *ConsoleLogReaper {
	return &ConsoleLogReaper{dir: dir, now: time.Now}
}

func (r *ConsoleLogReaper) Class() string      { return "console-log" }
func (r *ConsoleLogReaper) Scope() ReaperScope { return ScopeNodeLocal }

// NewLocalConsoleLogReaper builds the reaper over this node's runtime directory.
func (m *Manager) NewLocalConsoleLogReaper() *ConsoleLogReaper {
	return NewConsoleLogReaper(hostprocess.RuntimeDir())
}

// Sweep cuts oversized console files back to their newest consoleKeepBytes and
// removes retired ones that have sat for over an hour.
func (r *ConsoleLogReaper) Sweep(context.Context) (int, error) {
	reaped := 0
	live, err := filepath.Glob(filepath.Join(r.dir, "console-*.log"))
	if err != nil {
		return 0, err
	}
	for _, p := range live {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= consoleTrimAbove {
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
	now := r.now()
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
