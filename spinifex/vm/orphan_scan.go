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

// claimedQEMUPids reads every "<instance-id>.pid" file in runtimeDir whose
// instance ID is in known, returning the PIDs those records claim. QEMU
// itself writes these via -pidfile, so a claimed PID is one this manager
// launched and is actively tracking.
func claimedQEMUPids(runtimeDir string, known map[string]bool) map[int]bool {
	claimed := make(map[int]bool)
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return claimed
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}
		if id := strings.TrimSuffix(name, ".pid"); known[id] {
			if pid, err := utils.ReadPidFileFrom(runtimeDir, id); err == nil {
				claimed[pid] = true
			}
		}
	}
	return claimed
}

// findRecordlessQEMUOrphans returns live qemu-system PIDs that no pidfile in
// runtimeDir ties to an instance in known. This is the gap
// classifyRestoredInstances cannot see, since it only walks Snapshot().
func findRecordlessQEMUOrphans(procRoot, runtimeDir string, known map[string]bool) ([]int, error) {
	live, err := scanLiveQEMUPIDs(procRoot)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		return nil, nil
	}

	claimed := claimedQEMUPids(runtimeDir, known)
	var orphans []int
	for _, pid := range live {
		if !claimed[pid] {
			orphans = append(orphans, pid)
		}
	}
	return orphans, nil
}

// reportRecordlessQEMUOrphans logs, but never kills, a qemu-system process
// with no instance record: a false positive here (state not settled yet, or
// a process belonging to another daemon/tenant on a shared host) would
// destroy a running customer VM. Call only after Restore has finished
// loading and relaunching every known instance.
func (m *Manager) reportRecordlessQEMUOrphans() {
	known := make(map[string]bool)
	for _, instance := range m.Snapshot() {
		known[instance.ID] = true
	}

	orphans, err := findRecordlessQEMUOrphans(qemuProcRoot, utils.RuntimeDir(), known)
	if err != nil {
		slog.Warn("recordless QEMU orphan scan failed", "error", err)
		return
	}
	for _, pid := range orphans {
		slog.Warn("qemu-system process has no instance record on this daemon; not killing, verify manually",
			"pid", pid)
	}
}
