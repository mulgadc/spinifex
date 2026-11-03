package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

const (
	// consoleTrimAbove is the size past which a console log is cut back.
	consoleTrimAbove = 4 << 20
	// consoleKeepBytes is how much of the newest output survives a cut.
	consoleKeepBytes = 1 << 20
	// consoleRemoveAfter is how long a terminated or unknown instance's console
	// file lingers so the log shipper and a late GetConsoleOutput still see it.
	consoleRemoveAfter = time.Hour
)

// ConsoleLogEntry is what the reaper needs to know about an instance this node
// holds a record for.
type ConsoleLogEntry struct {
	State        InstanceState
	TerminatedAt time.Time
}

// ConsoleLogReaper caps and removes the serial console files under dir.
type ConsoleLogReaper struct {
	dir       string
	instances func() (map[string]ConsoleLogEntry, error)
	now       func() time.Time
}

var _ Reaper = (*ConsoleLogReaper)(nil)

// NewConsoleLogReaper builds a reaper over the console-<id>.log files in dir.
func NewConsoleLogReaper(dir string, instances func() (map[string]ConsoleLogEntry, error)) *ConsoleLogReaper {
	return &ConsoleLogReaper{dir: dir, instances: instances, now: time.Now}
}

func (r *ConsoleLogReaper) Class() string      { return "console-log" }
func (r *ConsoleLogReaper) Scope() ReaperScope { return ScopeNodeLocal }

// NewLocalConsoleLogReaper builds the reaper over this node's runtime directory.
func (m *Manager) NewLocalConsoleLogReaper() *ConsoleLogReaper {
	return NewConsoleLogReaper(hostprocess.RuntimeDir(), m.consoleLogEntries)
}

// consoleLogEntries lists every instance this node knows: its local records plus
// the shared stopped and terminated buckets.
func (m *Manager) consoleLogEntries() (map[string]ConsoleLogEntry, error) {
	entries := make(map[string]ConsoleLogEntry)
	if m.deps.StateStore != nil {
		stopped, err := m.deps.StateStore.ListStoppedInstances()
		if err != nil {
			return nil, fmt.Errorf("list stopped instances: %w", err)
		}
		for _, v := range stopped {
			entries[v.ID] = ConsoleLogEntry{State: StateStopped}
		}
		terminated, err := m.deps.StateStore.ListTerminatedInstances()
		if err != nil {
			return nil, fmt.Errorf("list terminated instances: %w", err)
		}
		for _, v := range terminated {
			entries[v.ID] = ConsoleLogEntry{State: StateTerminated, TerminatedAt: v.TerminatedAt}
		}
	}
	for _, v := range m.Snapshot() {
		entries[v.ID] = ConsoleLogEntry{State: m.Status(v), TerminatedAt: v.TerminatedAt}
	}
	return entries, nil
}

// Sweep removes the console files of instances gone for over an hour and cuts
// oversized ones back to their newest consoleKeepBytes. A record lookup that
// fails aborts the sweep, so an unreadable record set never looks like "all gone".
func (r *ConsoleLogReaper) Sweep(context.Context) (int, error) {
	instances, err := r.instances()
	if err != nil {
		return 0, fmt.Errorf("list instances: %w", err)
	}
	paths, err := filepath.Glob(filepath.Join(r.dir, "console-*.log"))
	if err != nil {
		return 0, err
	}
	now := r.now()
	reaped := 0
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "console-"), ".log")
		entry, known := instances[id]
		if r.expired(entry, known, info.ModTime(), now) {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				slog.Warn("vm/gc: remove console log failed", "instanceId", id, "err", err)
				continue
			}
			reaped++
			continue
		}
		if info.Size() > consoleTrimAbove {
			if err := trimConsoleLog(p); err != nil {
				slog.Warn("vm/gc: trim console log failed", "instanceId", id, "err", err)
				continue
			}
			reaped++
		}
	}
	return reaped, nil
}

// expired reports whether a console file has outlived its instance. Without a
// usable terminated-at stamp the file's own mtime decides, so no file is kept forever.
func (r *ConsoleLogReaper) expired(e ConsoleLogEntry, known bool, mtime, now time.Time) bool {
	if known && e.State != StateTerminated {
		return false
	}
	if known && !e.TerminatedAt.IsZero() {
		return now.Sub(e.TerminatedAt) > consoleRemoveAfter
	}
	return now.Sub(mtime) > consoleRemoveAfter
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
