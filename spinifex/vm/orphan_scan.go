package vm

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// qemuProcessPrefix matches both qemu-system-x86_64 and qemu-system-aarch64;
// /proc/<pid>/comm truncates at 15 bytes but this prefix is only 11.
const qemuProcessPrefix = "qemu-system"

// qemuProcRoot is the /proc mount scanned for live qemu-system processes.
// Overridden in tests to avoid depending on the real host process table.
var qemuProcRoot = "/proc"

// scanLiveQEMUPIDs walks procRoot for processes whose comm starts with
// qemuProcessPrefix. procRoot is a parameter so tests point it at a
// fabricated directory instead of spawning a real qemu-system binary.
func scanLiveQEMUPIDs(procRoot string) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}

	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(comm)), qemuProcessPrefix) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// pidFileOwners reads every "<instance-id>.pid" file in runtimeDir and
// returns the PID -> instance ID it names, regardless of whether that
// instance still exists in Snapshot(). QEMU itself writes these via
// -pidfile, so an entry here is a claim this daemon made at some point --
// not necessarily one it still stands behind.
func pidFileOwners(runtimeDir string) map[int]string {
	owners := make(map[int]string)
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return owners
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}
		id := strings.TrimSuffix(name, ".pid")
		if pid, err := utils.ReadPidFileFrom(runtimeDir, id); err == nil {
			owners[pid] = id
		}
	}
	return owners
}

// qemuOrphan is a live qemu-system PID with no matching instance in
// Snapshot(). hasPidFile distinguishes two very different situations: a
// pidfile this daemon itself wrote (instanceID is that pidfile's claim,
// positively this daemon's process) versus no pidfile at all (could be
// another tenant's process on a shared host).
type qemuOrphan struct {
	pid        int
	instanceID string
	hasPidFile bool
}

// classifyQEMUOrphans returns every live qemu-system PID not vouched for by
// a pidfile naming a known instance. This is the gap classifyRestoredInstances
// cannot see, since it only walks Snapshot().
func classifyQEMUOrphans(procRoot, runtimeDir string, known map[string]bool) ([]qemuOrphan, error) {
	live, err := scanLiveQEMUPIDs(procRoot)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		return nil, nil
	}

	owners := pidFileOwners(runtimeDir)
	var orphans []qemuOrphan
	for _, pid := range live {
		id, hasPidFile := owners[pid]
		if hasPidFile && known[id] {
			continue // legitimate, tracked instance
		}
		orphans = append(orphans, qemuOrphan{pid: pid, instanceID: id, hasPidFile: hasPidFile})
	}
	return orphans, nil
}

// reportRecordlessQEMUOrphans logs, but never kills, a qemu-system process
// with no instance record. A pidfile in runtimeDir naming an unknown
// instance positively identifies the process as this daemon's own stale
// orphan (logged distinctly, safe for an operator to reap by hand); no
// pidfile at all could belong to another tenant/daemon on a shared host, so
// it is logged as unowned. Neither case signals the process automatically:
// a false positive here would destroy a running customer VM. Call only
// after Restore has finished loading and relaunching every known instance.
func (m *Manager) reportRecordlessQEMUOrphans() {
	known := make(map[string]bool)
	for _, instance := range m.Snapshot() {
		known[instance.ID] = true
	}

	orphans, err := classifyQEMUOrphans(qemuProcRoot, utils.RuntimeDir(), known)
	if err != nil {
		slog.Warn("recordless QEMU orphan scan failed", "error", err)
		return
	}
	for _, o := range orphans {
		if o.hasPidFile {
			slog.Warn("qemu-system process has a stale spinifex pidfile with no matching instance record; not killing, safe to reap manually",
				"pid", o.pid, "instanceId", o.instanceID)
			continue
		}
		slog.Warn("qemu-system process has no spinifex pidfile at all; not killing, ownership unconfirmed",
			"pid", o.pid)
	}
}
