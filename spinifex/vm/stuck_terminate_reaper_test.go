package vm

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStuckTerminateReaper(t *testing.T, cleaner InstanceCleaner) (*StuckTerminateReaper, *fakeStateStore) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	m := NewManager()
	m.SetDeps(Deps{NodeID: "test-node", StateStore: store, InstanceCleaner: cleaner})
	return m.NewStuckTerminateReaper(), store
}

func TestStuckTerminateReaper(t *testing.T) {
	t.Run("force-completes a terminate wedged past the timeout, reclaiming DoT volume space", func(t *testing.T) {
		cleaner := &recordingInstanceCleaner{}
		reaper, store := newStuckTerminateReaper(t, cleaner)
		m := reaper.m

		// A live-but-wedged QEMU the dead-QEMU reconcile would never touch. Reap
		// the child concurrently so the force-kill's SIGKILL'd process leaves the
		// zombie state (and reads as not-alive) rather than lingering until Wait.
		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		pid := cmd.Process.Pid
		var wg sync.WaitGroup
		wg.Go(func() { _ = cmd.Wait() })

		const id = "i-wedged-live"
		require.NoError(t, utils.WritePidFile(id, pid))
		m.InsertIfAbsent(&VM{
			ID:             id,
			Status:         StateShuttingDown,
			ENIId:          "eni-1",
			ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
		})

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		wg.Wait()
		assert.Equal(t, 1, reaped, "a terminate wedged past the timeout must be force-completed")

		assert.False(t, utils.ProcessAlive(pid), "the wedged QEMU must be force-killed")
		_, ok := m.Get(id)
		assert.False(t, ok, "the finalized instance must leave the local running map")

		term, ok := store.terminated[id]
		require.True(t, ok, "the instance must be driven to the terminated bucket")
		assert.Equal(t, StateTerminated, term.Status)
		assert.Contains(t, cleaner.deleteVolumes, id, "DoT volume space must be reclaimed via the cleaner")
		assert.Equal(t, string(TeardownDone), term.Teardown[TeardownVolumes], "reclaimed volumes are done")
		assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownENI], "remaining ENI teardown is handed off")
	})

	t.Run("surfaces a failed volume reclaim for reaper handoff rather than blocking finalize", func(t *testing.T) {
		cleaner := &recordingInstanceCleaner{deleteVolumesErr: errors.New("predastore delete failed")}
		reaper, store := newStuckTerminateReaper(t, cleaner)
		m := reaper.m

		const id = "i-wedged-delete-fail"
		m.InsertIfAbsent(&VM{
			ID:             id,
			Status:         StateShuttingDown,
			ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
		})

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, reaped, "the instance is still finalized even when the volume delete fails")

		term := store.terminated[id]
		require.NotNil(t, term)
		assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownVolumes],
			"a failed reclaim must be stamped failed so TerminatedTeardownReaper retries it")
	})

	t.Run("a recently shutting-down instance is left to finish terminating", func(t *testing.T) {
		cleaner := &recordingInstanceCleaner{}
		reaper, _ := newStuckTerminateReaper(t, cleaner)
		m := reaper.m

		const id = "i-terminating"
		m.InsertIfAbsent(&VM{ID: id, Status: StateShuttingDown, ShuttingDownAt: time.Now()})

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "a terminate still within the timeout must not be force-completed")
		assert.Empty(t, cleaner.deleteVolumes, "no volume must be reclaimed for a healthy in-progress terminate")
		_, ok := m.Get(id)
		assert.True(t, ok, "the instance must stay in the running map")
	})

	t.Run("a shutting-down instance with no timestamp is never force-completed", func(t *testing.T) {
		cleaner := &recordingInstanceCleaner{}
		reaper, _ := newStuckTerminateReaper(t, cleaner)
		m := reaper.m

		const id = "i-no-timestamp"
		m.InsertIfAbsent(&VM{ID: id, Status: StateShuttingDown}) // ShuttingDownAt zero

		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Zero(t, reaped, "an unbounded record (no timestamp) must be left untouched")
		_, ok := m.Get(id)
		assert.True(t, ok)
	})
}
