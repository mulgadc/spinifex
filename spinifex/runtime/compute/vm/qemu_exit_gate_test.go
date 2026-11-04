//test:in-package — replaces the unexported QEMU kill and drives both reapers
//over the shared manager and state-store doubles.

package vm

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveQEMU starts a stand-in QEMU process recorded in the instance's PID file.
// The returned stop kills and reaps it, so it reads as not alive afterwards.
func liveQEMU(t *testing.T, id string) (pid int, stop func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	var wg sync.WaitGroup
	wg.Go(func() { _ = cmd.Wait() })
	require.NoError(t, hostprocess.WritePidFile(id, cmd.Process.Pid))
	stop = func() { _ = cmd.Process.Kill(); wg.Wait() }
	t.Cleanup(stop)
	return cmd.Process.Pid, stop
}

// TestS7_StuckTerminateNeverDeletesVolumesWhileQEMUMayBeAlive enforces
// ADR-0003:S7: an automatic terminate path must not delete a volume the guest
// may still be writing. When the kill cannot prove QEMU gone, neither the
// stuck-terminate backstop nor the teardown reaper may call DeleteVolumes.
func TestS7_StuckTerminateNeverDeletesVolumesWhileQEMUMayBeAlive(t *testing.T) {
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	killErr := errors.New("kill failed")
	orig := forceKillQEMU
	forceKillQEMU = func(int, time.Duration) error { return killErr }
	t.Cleanup(func() { forceKillQEMU = orig })

	cleaner := &recordingInstanceCleaner{}
	stuck, store := newStuckTerminateReaper(t, cleaner)
	m := stuck.m

	const id = "i-kill-fails"
	_, stop := liveQEMU(t, id)
	m.InsertIfAbsent(&VM{
		ID:             id,
		Status:         StateShuttingDown,
		LastNode:       "test-node",
		ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
	})

	_, err := stuck.Sweep(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cleaner.deleteVolumes, "no DeleteVolumes while QEMU may be alive")

	term := store.terminated[id]
	require.NotNil(t, term, "the record is left terminated for the teardown reaper")
	assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownQEMU], "QEMU teardown stays failed")
	assert.Equal(t, string(TeardownFailed), term.Teardown[TeardownVolumes], "volumes stay outstanding")

	teardown := m.NewTerminatedTeardownReaper()
	for range 2 {
		_, err = teardown.Sweep(context.Background())
		require.NoError(t, err)
	}
	assert.Empty(t, cleaner.deleteVolumes, "the reaper must not delete volumes while QEMU is alive")
	assert.Equal(t, string(TeardownFailed), store.terminated[id].Teardown[TeardownQEMU])

	stop()
	_, err = teardown.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{id}, cleaner.deleteVolumes, "QEMU proven gone: the reaper deletes the volumes")
	assert.Equal(t, string(TeardownDone), store.terminated[id].Teardown[TeardownQEMU])
	assert.Equal(t, string(TeardownDone), store.terminated[id].Teardown[TeardownVolumes])
}

// TestS7_StuckTerminateDeletesVolumesOnceQEMUIsProvenGone is the positive case
// beside the invariant: a successful kill lets the backstop reclaim volumes.
func TestS7_StuckTerminateDeletesVolumesOnceQEMUIsProvenGone(t *testing.T) {
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	cleaner := &recordingInstanceCleaner{}
	stuck, store := newStuckTerminateReaper(t, cleaner)

	const id = "i-kill-works"
	pid, _ := liveQEMU(t, id)
	stuck.m.InsertIfAbsent(&VM{
		ID:             id,
		Status:         StateShuttingDown,
		ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
	})

	_, err := stuck.Sweep(context.Background())
	require.NoError(t, err)
	assert.False(t, hostprocess.ProcessAlive(pid))
	assert.Equal(t, []string{id}, cleaner.deleteVolumes)
	assert.Equal(t, string(TeardownDone), store.terminated[id].Teardown[TeardownQEMU])
}
