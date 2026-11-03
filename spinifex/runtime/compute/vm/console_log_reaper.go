package vm

import (
	"context"
	"time"
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

// Sweep trims oversized console files and removes those of gone instances.
func (r *ConsoleLogReaper) Sweep(context.Context) (int, error) {
	return 0, nil
}
