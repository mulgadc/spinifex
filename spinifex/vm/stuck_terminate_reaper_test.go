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

	t.Run("force-completes a VM whose ShuttingDownAt was only stamped by the persistence-failure path", func(t *testing.T) {
		// This is the case the backstop existed to cover: on a full disk the
		// state write fails, but transitionWithPrecheck must still stamp
		// ShuttingDownAt so this VM is not skipped forever.
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		cleaner := &recordingInstanceCleaner{}
		store := newFakeStateStore()
		persistErr := errors.New("no space left on device")
		var callCount int
		var m *Manager
		m = NewManagerWithDeps(Deps{
			NodeID:          "test-node",
			StateStore:      store,
			InstanceCleaner: cleaner,
			// Only the first call (the shutting-down transition below) fails,
			// modelling the disk being full at that moment; the reaper's own
			// later transition to terminated succeeds normally.
			TransitionState: func(v *VM, target InstanceState) error {
				m.Inspect(v, func(vv *VM) { vv.Status = target })
				callCount++
				if callCount == 1 {
					return persistErr
				}
				return nil
			},
		})

		const id = "i-wedged-persist-fail"
		instance := &VM{ID: id, Status: StateRunning}
		m.Insert(instance)

		err := m.transitionWithPrecheck(instance, StateShuttingDown)
		require.ErrorIs(t, err, persistErr, "the persistence error must still reach the caller")
		require.False(t, instance.ShuttingDownAt.IsZero(),
			"the failure path must stamp ShuttingDownAt for the reaper to ever see this instance")

		// Back-date the stamp instead of sleeping so the reaper sees a
		// timed-out VM without a real wait.
		m.Inspect(instance, func(v *VM) {
			v.ShuttingDownAt = time.Now().Add(-(stuckTerminateTimeout + time.Minute))
		})

		reaper := m.NewStuckTerminateReaper()
		reaped, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, reaped,
			"a VM stamped only via the persistence-failure path must still be force-completed once past the timeout")

		_, ok := m.Get(id)
		assert.False(t, ok, "the finalized instance must leave the local running map")
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
