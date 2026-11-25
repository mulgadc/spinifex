package preflight

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	hostcommand "github.com/mulgadc/spinifex/spinifex/runtime/host/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemdMainPIDFor is the real systemctl lookup, which TestMain replaces so
// CheckHostAt does not run systemctl once per unit in every test.
var systemdMainPIDFor func(string) (int, error)

// fakeProc builds a root containing a binary and a /proc/<pid>/exe symlink to
// it, standing in for the parts of procfs the check reads.
func fakeProc(t *testing.T, pid int, binPath string, target string) string {
	t.Helper()
	root := t.TempDir()

	full := filepath.Join(root, binPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte("binary contents"), 0o600))

	procDir := filepath.Join(root, "proc", strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(procDir, 0o750))

	// The link records a host-absolute path; the check resolves it under root.
	if target == "" {
		target = "/" + binPath
	}
	require.NoError(t, os.Symlink(target, filepath.Join(procDir, "exe")))
	return root
}

// stubInodes makes both stat calls resolve through the fake root, and lets a
// test model a swapped binary by giving the two paths different inodes.
func stubInodes(t *testing.T, root string, override map[string]uint64) {
	t.Helper()
	orig := inodeOf
	t.Cleanup(func() { inodeOf = orig })

	inodeOf = func(path string) (uint64, error) {
		if ino, ok := override[path]; ok {
			return ino, nil
		}
		// Everything not overridden resolves as one file, so an unswapped
		// binary compares equal.
		if _, err := os.Lstat(path); err != nil {
			return 0, err
		}
		return 1, nil
	}
}

// stubMainPID reports the given pid for one unit and "no such unit" for the rest.
func stubMainPID(t *testing.T, unit string, pid int) {
	t.Helper()
	orig := mainPIDFor
	t.Cleanup(func() { mainPIDFor = orig })

	mainPIDFor = func(u string) (int, error) {
		if u == unit {
			return pid, nil
		}
		return 0, nil
	}
}

func TestCheckRunningBinaries(t *testing.T) {
	const (
		unit    = "spinifex-daemon.service"
		pid     = 4242
		binPath = "usr/local/bin/spx"
	)

	t.Run("running the binary that is on disk is OK", func(t *testing.T) {
		root := fakeProc(t, pid, binPath, "")
		stubInodes(t, root, nil)
		stubMainPID(t, unit, pid)

		results := checkRunningBinaries(root)
		require.Len(t, results, 1)
		assert.Equal(t, unit, results[0].Path)
		assert.Equal(t, "service", results[0].Kind)
		assert.Equal(t, OK, results[0].Status)
	})

	t.Run("a deleted binary is reported stale", func(t *testing.T) {
		root := fakeProc(t, pid, binPath, "/usr/local/bin/spx"+deletedSuffix)
		stubMainPID(t, unit, pid)

		results := checkRunningBinaries(root)
		require.Len(t, results, 1)
		assert.Equal(t, Stale, results[0].Status)
		assert.Contains(t, results[0].Detail, "deleted")
		assert.Contains(t, results[0].Detail, "restart spinifex.target")
	})

	t.Run("a binary removed outright is reported stale", func(t *testing.T) {
		root := fakeProc(t, pid, binPath, "")
		require.NoError(t, os.Remove(filepath.Join(root, binPath)))
		stubInodes(t, root, nil)
		stubMainPID(t, unit, pid)

		results := checkRunningBinaries(root)
		require.Len(t, results, 1)
		assert.Equal(t, Stale, results[0].Status)
		assert.Contains(t, results[0].Detail, "no longer on disk")
	})

	t.Run("a replaced binary at the same path is reported stale", func(t *testing.T) {
		root := fakeProc(t, pid, binPath, "")

		// The path resolves to a new inode while the process still executes the
		// old one — a swap that leaves no (deleted) marker behind.
		stubInodes(t, root, map[string]uint64{
			filepath.Join(root, binPath):                          200,
			filepath.Join(root, "proc", strconv.Itoa(pid), "exe"): 100,
		})
		stubMainPID(t, unit, pid)

		results := checkRunningBinaries(root)
		require.Len(t, results, 1)
		assert.Equal(t, Stale, results[0].Status)
		assert.Contains(t, results[0].Detail, "older copy")
	})

	t.Run("a unit that is not running is skipped", func(t *testing.T) {
		root := fakeProc(t, pid, binPath, "")
		stubMainPID(t, "no-unit-runs.service", pid)

		assert.Empty(t, checkRunningBinaries(root))
	})

	t.Run("an unreadable proc entry is reported rather than passed", func(t *testing.T) {
		root := t.TempDir()
		stubMainPID(t, unit, pid)

		results := checkRunningBinaries(root)
		require.Len(t, results, 1)
		assert.Equal(t, Missing, results[0].Status)
		assert.Contains(t, results[0].Detail, "cannot read its executable")
	})
}

// A unit the host has never loaded must not look like a running process,
// or checkRunningBinaries would report it on every node that lacks it.
func TestMainPIDForUnloadedUnit(t *testing.T) {
	pid, err := systemdMainPIDFor("spinifex-preflight-test-absent.service")
	if err != nil {
		t.Logf("systemctl unavailable on this host: %v", err)
		return
	}
	assert.Zero(t, pid)
}

// TestServiceUnitsAreServicesOnly guards the enumeration: a timer or the
// target has no MainPID of its own, so including one would report a spurious
// problem on every node.
func TestServiceUnitsAreServicesOnly(t *testing.T) {
	units := serviceUnits()
	require.NotEmpty(t, units)

	for _, u := range units {
		assert.Equal(t, ".service", filepath.Ext(u), "%s is not a .service unit", u)
	}
	assert.Contains(t, units, "spinifex-daemon.service")
	assert.NotContains(t, units, "spinifex.target")
}

// The pre-swap gate must not see services on the old binary, but must still
// fail on a bad asset; the full check reports both.
func TestCheckHostAt_RunningBinariesSelectable(t *testing.T) {
	const (
		unit = "spinifex-daemon.service"
		pid  = 4242
	)
	root := fakeProc(t, pid, "usr/local/bin/spx", "/usr/local/bin/spx"+deletedSuffix)
	stubMainPID(t, unit, pid)
	helper := hostcommand.EndpointSysctlHelper
	writeHelper(t, root, helper, []byte("not the canonical helper body\n"))

	serviceRows := func(results []Result) []Result {
		var rows []Result
		for _, r := range results {
			if r.Kind == "service" {
				rows = append(rows, r)
			}
		}
		return rows
	}

	assets := checkHostAt(root, false)
	assert.Empty(t, serviceRows(assets))
	assert.Equal(t, Stale, findResult(t, assets, helper).Status)
	assert.True(t, HasProblem(assets))

	full := CheckHostAt(root)
	rows := serviceRows(full)
	require.Len(t, rows, 1)
	assert.Equal(t, unit, rows[0].Path)
	assert.Equal(t, Stale, rows[0].Status)
	assert.Equal(t, Stale, findResult(t, full, helper).Status)
}
