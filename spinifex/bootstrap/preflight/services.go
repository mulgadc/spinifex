package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/systemd"
)

// deletedSuffix is what the kernel appends to /proc/<pid>/exe once the
// executable's directory entry is gone, which is what a binary swap under a
// running service leaves behind.
const deletedSuffix = " (deleted)"

// mainPIDFor is overridable so tests never shell out to a real systemd.
var mainPIDFor = func(unit string) (int, error) {
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// serviceUnits returns the .service units this build ships, sorted so the
// report is stable. Timers and the target have no MainPID of their own.
func serviceUnits() []string {
	names := make([]string, 0, len(systemd.Units))
	for name := range systemd.Units {
		if strings.HasSuffix(name, ".service") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// checkRunningBinaries reports units whose running process is executing a
// binary that is no longer the one on disk. Replacing a binary under a running
// service does not move the service onto it: it keeps executing the replaced
// inode until it restarts, so a node can serve a mixture of two builds.
func checkRunningBinaries(root string) []Result {
	var results []Result
	for _, unit := range serviceUnits() {
		pid, err := mainPIDFor(unit)
		if err != nil {
			// A unit this build ships but this host does not run is normal:
			// services are per-node, and systemctl reports nothing for a unit
			// that was never installed.
			continue
		}
		if pid == 0 {
			continue
		}
		results = append(results, checkUnitBinary(root, unit, pid))
	}
	return results
}

func checkUnitBinary(root, unit string, pid int) Result {
	exeLink := filepath.Join(root, "proc", strconv.Itoa(pid), "exe")

	target, err := os.Readlink(exeLink)
	if err != nil {
		return Result{
			Path:   unit,
			Kind:   "service",
			Status: Missing,
			Detail: fmt.Sprintf("pid %d: cannot read its executable: %v", pid, err),
		}
	}

	if deleted, ok := strings.CutSuffix(target, deletedSuffix); ok {
		return Result{
			Path:   unit,
			Kind:   "service",
			Status: Stale,
			Detail: fmt.Sprintf("pid %d is running a deleted %s — restart spinifex.target", pid, deleted),
		}
	}

	// A binary removed outright leaves a link to a path that no longer
	// resolves, which is the same skew reported without the marker.
	onDisk, err := inodeOf(filepath.Join(root, target))
	if err != nil {
		return Result{
			Path:   unit,
			Kind:   "service",
			Status: Stale,
			Detail: fmt.Sprintf("pid %d is running %s, which is no longer on disk — restart spinifex.target", pid, target),
		}
	}

	// The marker is absent when the running inode still has another link, so
	// compare inodes too: the path resolving to a different file than the one
	// being executed is the same skew, unsignposted.
	running, err := inodeOf(exeLink)
	if err != nil {
		return Result{
			Path:   unit,
			Kind:   "service",
			Status: Missing,
			Detail: fmt.Sprintf("pid %d: cannot stat its executable: %v", pid, err),
		}
	}
	if running != onDisk {
		return Result{
			Path:   unit,
			Kind:   "service",
			Status: Stale,
			Detail: fmt.Sprintf("pid %d is running an older copy of %s — restart spinifex.target", pid, target),
		}
	}

	return Result{Path: unit, Kind: "service", Status: OK}
}

// inodeOf resolves a path to the inode behind it, following symlinks so
// /proc/<pid>/exe reports the executable rather than the link. Overridable so
// tests can model a swap without a second real file.
var inodeOf = func(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no inode available on this platform", path)
	}
	return st.Ino, nil
}
