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

func TestPidFileOwners(t *testing.T) {
	runtimeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-gone.pid"), []byte("999"), 0o600))

	owners := pidFileOwners(runtimeDir)

	assert.Equal(t, "i-known", owners[111], "a pidfile's PID maps to the instance ID it names")
	assert.Equal(t, "i-gone", owners[999],
		"pidFileOwners is not filtered by known -- it returns every claim on disk, known or not")
}

func TestClassifyQEMUOrphans(t *testing.T) {
	t.Run("known instance's QEMU is left alone", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64")
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))

		orphans, err := classifyQEMUOrphans(procRoot, runtimeDir, map[string]bool{"i-known": true})

		require.NoError(t, err)
		assert.Empty(t, orphans, "a QEMU process claimed by a known instance's pidfile is not an orphan")
	})

	t.Run("recordless QEMU process with no pidfile at all", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64")
		// No pidfile at all ties PID 222 to any instance.

		orphans, err := classifyQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		require.Len(t, orphans, 1)
		assert.Equal(t, 222, orphans[0].pid)
		assert.False(t, orphans[0].hasPidFile, "no pidfile claims this PID -- ownership is unconfirmed")
	})

	t.Run("stale pidfile naming an unknown instance is classified as this daemon's own", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 333, "qemu-system-aarch64")
		// Pidfile exists but names an instance this manager has no record of.
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-gone.pid"), []byte("333"), 0o600))

		orphans, err := classifyQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		require.Len(t, orphans, 1)
		assert.Equal(t, 333, orphans[0].pid)
		assert.True(t, orphans[0].hasPidFile, "a pidfile this daemon wrote positively identifies the process")
		assert.Equal(t, "i-gone", orphans[0].instanceID)
	})

	t.Run("mixed: known left alone, recordless-with-pidfile and recordless-without kept separate", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64") // known
		fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64") // stale pidfile
		fakeProcEntry(t, procRoot, 333, "qemu-system-x86_64") // no pidfile
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-gone.pid"), []byte("222"), 0o600))

		orphans, err := classifyQEMUOrphans(procRoot, runtimeDir, map[string]bool{"i-known": true})

		require.NoError(t, err)
		require.Len(t, orphans, 2)
		byPID := map[int]qemuOrphan{}
		for _, o := range orphans {
			byPID[o.pid] = o
		}
		assert.True(t, byPID[222].hasPidFile)
		assert.Equal(t, "i-gone", byPID[222].instanceID)
		assert.False(t, byPID[333].hasPidFile)
	})

	t.Run("no live qemu-system processes returns no orphans", func(t *testing.T) {
		procRoot, runtimeDir := t.TempDir(), t.TempDir()
		fakeProcEntry(t, procRoot, 444, "bash")

		orphans, err := classifyQEMUOrphans(procRoot, runtimeDir, map[string]bool{})

		require.NoError(t, err)
		assert.Empty(t, orphans)
	})
}

// TestReportRecordlessQEMUOrphans covers the Manager-level entry point used
// by Restore: a known instance's QEMU must not be reported, and the two
// orphan classes (own stale pidfile vs no pidfile at all) must be logged
// distinctly so an operator can tell them apart. Neither is signalled --
// there is no kill path in this function to assert against.
func TestReportRecordlessQEMUOrphans(t *testing.T) {
	procRoot := t.TempDir()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	origProcRoot := qemuProcRoot
	qemuProcRoot = procRoot
	t.Cleanup(func() { qemuProcRoot = origProcRoot })

	fakeProcEntry(t, procRoot, 111, "qemu-system-x86_64") // claimed by i-known
	fakeProcEntry(t, procRoot, 222, "qemu-system-x86_64") // stale pidfile, unknown instance
	fakeProcEntry(t, procRoot, 333, "qemu-system-x86_64") // no pidfile at all
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-known.pid"), []byte("111"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "i-gone.pid"), []byte("222"), 0o600))

	m := NewManager()
	m.Replace(map[string]*VM{"i-known": {ID: "i-known", Status: StateRunning}})

	buf := captureSlogRestore(t)
	m.reportRecordlessQEMUOrphans()
	output := buf.String()

	assert.NotContains(t, output, "pid=111", "the known instance's QEMU PID must not be reported as an orphan")
	assert.Contains(t, output, "pid=222", "the stale-pidfile orphan must be reported")
	assert.Contains(t, output, "i-gone", "the stale pidfile's instance ID must be logged for the operator")
	assert.Contains(t, output, "pid=333", "the no-pidfile orphan must be reported")
	assert.Contains(t, output, "not killing", "neither orphan class is signalled")
}
