package vm

import (
	"errors"
	"os/exec"
	"sync"
	"testing"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lease that lapsed with no peer holding the volume fences the export, and
// the guest is paused rather than killed, so it can resume if its node takes
// the lease back. It stays running: nothing has stopped it.
func TestStalledFencePausesTheGuest(t *testing.T) {
	m, instance, pid, qmpCalls, _ := fencedGuest(t, "vol-stalledfence")

	require.Equal(t, instance.ID, m.PauseForFence(t.Context(), "vol-stalledfence", "lease unconfirmed, no peer"))
	m.goroutineWg.Wait()

	assert.Truef(t, hostprocess.ProcessAlive(pid),
		"the guest (pid %d) was killed although no other node holds its volume", pid)
	assert.Equal(t, 1, qmpCalls("stop"), "the guest must be paused while its node tries to take the lease back")
	assert.Equal(t, StateRunning, m.Status(instance), "a paused guest has not been stopped")
}

// A stop for a paused guest kills the process the pause pinned.
func TestAPausedGuestIsKilledByALaterStop(t *testing.T) {
	m, _, pid, qmpCalls, _ := fencedGuest(t, "vol-pausedstop")
	m.PauseForFence(t.Context(), "vol-pausedstop", "lease unconfirmed, no peer")

	m.FenceVolume(t.Context(), "vol-pausedstop", "the pause window ended")
	waitFenceTeardown(t, m)

	assert.Falsef(t, hostprocess.ProcessAlive(pid), "the paused guest (pid %d) survived the stop", pid)
	assert.Zero(t, qmpCalls("system_powerdown"), "a fenced guest has no disk to flush")
}

// The kill goes through the pinned process, never the PID file. A guest
// relaunched here under a new pid after the paused one exited is untouched.
func TestAStopNeverSignalsAProcessThePauseDidNotPin(t *testing.T) {
	m, instance, pid, _, _ := fencedGuest(t, "vol-pausedreuse")
	m.PauseForFence(t.Context(), "vol-pausedreuse", "lease unconfirmed, no peer")

	require.NoError(t, hostprocess.ForceKillProcess(pid, fencedGuestTeardownBound))
	successor := exec.Command("sleep", "300")
	require.NoError(t, successor.Start())
	var reaped sync.WaitGroup
	reaped.Go(func() { _ = successor.Wait() })
	t.Cleanup(func() { _ = successor.Process.Kill(); reaped.Wait() })
	require.NoError(t, hostprocess.WritePidFile(instance.ID, successor.Process.Pid))

	m.FenceVolume(t.Context(), "vol-pausedreuse", "the pause window ended")
	waitFenceTeardown(t, m)

	assert.Truef(t, hostprocess.ProcessAlive(successor.Process.Pid),
		"the stop killed pid %d, which the PID file named but the pause never pinned", successor.Process.Pid)
	current, err := hostprocess.ReadPidFile(instance.ID)
	require.NoError(t, err)
	assert.Equal(t, successor.Process.Pid, current, "the PID file names the successor and must be left alone")
}

// A resume continues the guest only once its precondition holds, and a failed
// precondition leaves it paused.
func TestAResumeContinuesOnlyAfterItsPrecondition(t *testing.T) {
	m, instance, _, qmpCalls, _ := fencedGuest(t, "vol-pausedresume")
	m.PauseForFence(t.Context(), "vol-pausedresume", "lease unconfirmed, no peer")

	moved := errors.New("the instance is assigned elsewhere")
	_, err := m.ResumeAfterFence(t.Context(), "vol-pausedresume", func(string) error { return moved })
	require.ErrorIs(t, err, moved)
	assert.Zero(t, qmpCalls("cont"), "a resume whose precondition failed must not continue the guest")

	m.PauseForFence(t.Context(), "vol-pausedresume", "lease unconfirmed, no peer")
	id, err := m.ResumeAfterFence(t.Context(), "vol-pausedresume", func(string) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, instance.ID, id)
	assert.Equal(t, 1, qmpCalls("cont"), "the guest is continued once its volume is back")

	_, err = m.ResumeAfterFence(t.Context(), "vol-pausedresume", nil)
	assert.ErrorIs(t, err, ErrNotFencePaused, "a guest that is not paused for a fence cannot be resumed")
}

// The heartbeat's I/O-error resume must leave a fence-paused guest alone: its
// export is gone until the fence resumes it.
func TestAFencePausedGuestIsNotResumedByTheIOErrorPath(t *testing.T) {
	m, instance, _, qmpCalls, _ := fencedGuest(t, "vol-pausedioerr")
	m.PauseForFence(t.Context(), "vol-pausedioerr", "lease unconfirmed, no peer")

	for range 3 {
		m.resumeAfterIOError(t.Context(), instance)
	}
	assert.Zero(t, qmpCalls("cont"), "a guest paused for a fence was continued with no disk behind it")
}
