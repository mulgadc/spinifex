package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProcEntry creates procRoot/<pid>/comm containing name, mimicking the
// /proc layout scanLiveQEMUPIDs reads, without spawning a real process.
func fakeProcEntry(t *testing.T, procRoot string, pid int, name string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "comm"), []byte(name+"\n"), 0o644))
}

func TestScanLiveQEMUPIDs(t *testing.T) {
	procRoot := t.TempDir()
	fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64")
	fakeProcEntry(t, procRoot, 222, "qemu-system-aarch64")
	fakeProcEntry(t, procRoot, 333, "bash")
	// Non-numeric /proc entries (self, curproc, etc.) must not crash the scan.
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "self"), 0o755))

	pids, err := scanLiveQEMUPIDs(procRoot)

	require.NoError(t, err)
	assert.ElementsMatch(t, []int{111, 222}, pids, "only qemu-system* processes are returned")
}

func TestScanLiveQEMUPIDs_MissingProcRoot(t *testing.T) {
	_, err := scanLiveQEMUPIDs(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.Error(t, err, "a missing proc root must surface an error, not a silent empty result")
}

func TestClaimedQEMUPids(t *testing.T) {
	runtimeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-unknown.pid"), []byte("999"), 0o600))

	claimed := claimedQEMUPids(runtimeDir, map[string]bool{"i-known": true})

	assert.True(t, claimed[111], "a pidfile naming a known instance must claim its PID")
	assert.False(t, claimed[999], "a pidfile naming an instance absent from known must not claim its PID")
}

func TestFindRecordlessQEMUOrphans(t *testing.T) {
	t.Run("known instance's QEMU is left alone", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64")
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))

		orphans, err := findRecordlessQEMUOrphans(procRoot, runtimeDir, map[string]bool{"i-known": true})

		require.NoError(t, err)
		assert.Empty(t, orphans, "a QEMU process claimed by a known instance's pidfile is not an orphan")
	})

	t.Run("recordless QEMU process is detected", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64")
		// No pidfile at all ties PID 222 to any instance.

		orphans, err := findRecordlessQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		assert.Equal(t, []int{222}, orphans, "a qemu-system process with no owning pidfile must be reported")
	})

	t.Run("stale pidfile naming an unknown instance still surfaces the orphan", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 333, "qemu-system-aarch64")
		// Pidfile exists but names an instance this manager has no record of.
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-gone.pid"), []byte("333"), 0o600))

		orphans, err := findRecordlessQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		assert.Equal(t, []int{333}, orphans,
			"a pidfile naming an instance absent from Snapshot() must not suppress the orphan report")
	})

	t.Run("mixed: known left alone, recordless reported", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64")
		fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64")
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))

		orphans, err := findRecordlessQEMUOrphans(procRoot, runtimeDir, map[string]bool{"i-known": true})

		require.NoError(t, err)
		assert.Equal(t, []int{222}, orphans, "the known instance's PID must be excluded, the recordless one kept")
	})

	t.Run("no live qemu-system processes returns no orphans", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 444, "bash")

		orphans, err := findRecordlessQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		assert.Empty(t, orphans)
	})
}

// TestReportRecordlessQEMUOrphans covers the Manager-level entry point used
// by Restore: a known instance's QEMU must not be reported, and a recordless
// one must be logged, never signalled. There is no kill path in this
// function to assert against directly, so the log content is the contract.
func TestReportRecordlessQEMUOrphans(t *testing.T) {
	procRoot := t.TempDir()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	origProcRoot := qemuProcRoot
	qemuProcRoot = procRoot
	t.Cleanup(func() { qemuProcRoot = origProcRoot })

	fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64") // claimed by i-known
	fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64") // recordless
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))

	m := NewManager()
	m.Replace(map[string]*VM{"i-known": {ID: "i-known", Status: StateRunning}})

	buf := captureSlogRestore(t)
	m.reportRecordlessQEMUOrphans()
	output := buf.String()

	assert.NotContains(t, output, "pid=111", "the known instance's QEMU PID must not be reported as an orphan")
	assert.Contains(t, output, "pid=222", "the recordless QEMU PID must be reported")
	assert.Contains(t, output, "not killing", "the recordless process must be logged, not signalled")
}
