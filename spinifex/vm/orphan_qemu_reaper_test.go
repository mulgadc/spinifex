package vm

import (
	"context"
	"os/exec"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOrphanReaperManager(t *testing.T) (*OrphanQEMUReaper, *fakeStateStore) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	m := NewManager()
	m.SetDeps(Deps{NodeID: "test-node", StateStore: store})
	return m.NewOrphanQEMUReaper(), store
}

func TestOrphanQEMUReaper(t *testing.T) {
	t.Run("kills live QEMU for a terminated instance and removes its PID file", func(t *testing.T) {
		reaper, store := newOrphanReaperManager(t)

		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid
		var wg sync.WaitGroup
		wg.Go(func() { _ = cmd.Wait() })

		const id = "i-orphan-term"
		require.NoError(t, utils.WritePidFile(id, pid))
		store.terminated[id] = &VM{ID: id, Status: StateTerminated}

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		wg.Wait()

		assert.Equal(t, 1, reaped, "the orphan QEMU for a terminated instance must be reaped")
		assert.False(t, utils.ProcessAlive(pid),
			"a terminated instance must have no surviving process — otherwise it holds OVN ports (siv-476)")
		_, perr := utils.ReadPidFile(id)
		assert.Error(t, perr, "the PID file must be removed after reaping")
	})

	t.Run("no PID file for a terminated instance is a no-op", func(t *testing.T) {
		reaper, store := newOrphanReaperManager(t)
		store.terminated["i-term-elsewhere"] = &VM{ID: "i-term-elsewhere", Status: StateTerminated}

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "an instance whose process lives on another node must not be reaped here")
	})

	t.Run("stale PID file (dead process) is cleaned without counting as a reap", func(t *testing.T) {
		reaper, store := newOrphanReaperManager(t)
		const id = "i-term-stale"
		require.NoError(t, utils.WritePidFile(id, 999999)) // dead pid
		store.terminated[id] = &VM{ID: id, Status: StateTerminated}

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "a dead process is not a reap")
		_, perr := utils.ReadPidFile(id)
		assert.Error(t, perr, "the stale PID file must be cleaned up")
	})

	t.Run("a running instance with a live process is never touched", func(t *testing.T) {
		reaper, _ := newOrphanReaperManager(t)

		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

		// PID file exists but the instance is NOT in the terminated bucket.
		require.NoError(t, utils.WritePidFile("i-running", pid))

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "only terminated instances are candidates")
		assert.True(t, utils.ProcessAlive(pid), "a non-terminated instance's process must survive")
	})

	t.Run("reconciles a wedged shutting-down instance whose QEMU vanished", func(t *testing.T) {
		reaper, store := newOrphanReaperManager(t)
		m := reaper.m

		const id = "i-wedged"
		// A terminate transitioned it to shutting-down then wedged; QEMU is gone
		// (no PID file), and it never migrated to the terminated bucket.
		m.InsertIfAbsent(&VM{ID: id, Status: StateShuttingDown, ENIId: "eni-1"})

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, reaped, "a wedged shutting-down instance with a dead QEMU must be finalized")

		_, ok := m.Get(id)
		assert.False(t, ok, "the reconciled instance must leave the local running map")

		term, ok := store.terminated[id]
		require.True(t, ok, "the instance must be written to the terminated bucket")
		assert.Equal(t, StateTerminated, term.Status, "it must be finalized to terminated")
		assert.Equal(t, string(TeardownDone), term.Teardown[TeardownQEMU],
			"QEMU is confirmed gone, so its teardown is done")
		assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownVolumes],
			"outstanding volume teardown must be handed to TerminatedTeardownReaper")
		assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownENI],
			"the instance's ENI teardown must be handed off too")
	})

	t.Run("a shutting-down instance with a live QEMU is left to finish terminating", func(t *testing.T) {
		reaper, _ := newOrphanReaperManager(t)
		m := reaper.m

		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

		const id = "i-terminating"
		m.InsertIfAbsent(&VM{ID: id, Status: StateShuttingDown})
		require.NoError(t, utils.WritePidFile(id, pid))

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "a live in-progress terminate must not be reconciled")
		_, ok := m.Get(id)
		assert.True(t, ok, "the instance must stay in the running map for its own terminate to finish")
	})
}
