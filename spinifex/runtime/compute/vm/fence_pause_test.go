package vm

import (
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lease that lapsed with no peer holding the volume fences the export, and
// the guest is paused rather than killed, so it can resume if its node takes
// the lease back. It stays running: nothing has stopped it.
func TestStalledFencePausesTheGuest(t *testing.T) {
	m, instance, pid, qmpCalls, _ := fencedGuest(t, "vol-stalledfence")

	require.Equal(t, instance.ID, m.PauseForFence(t.Context(), "vol-stalledfence", "lease unconfirmed, no peer", 7, 0))
	m.goroutineWg.Wait()

	assert.Truef(t, hostprocess.ProcessAlive(pid),
		"the guest (pid %d) was killed although no other node holds its volume", pid)
	assert.Equal(t, 1, qmpCalls("stop"), "the guest must be paused while its node tries to take the lease back")
	assert.Equal(t, StateRunning, m.Status(instance), "a paused guest has not been stopped")
}

// A stop for a paused guest kills the process the pause pinned.
func TestAPausedGuestIsKilledByALaterStop(t *testing.T) {
	m, _, pid, qmpCalls, _ := fencedGuest(t, "vol-pausedstop")
	m.PauseForFence(t.Context(), "vol-pausedstop", "lease unconfirmed, no peer", 7, 0)

	m.FenceVolume(t.Context(), "vol-pausedstop", "the pause window ended")
	waitFenceTeardown(t, m)

	assert.Falsef(t, hostprocess.ProcessAlive(pid), "the paused guest (pid %d) survived the stop", pid)
	assert.Zero(t, qmpCalls("system_powerdown"), "a fenced guest has no disk to flush")
}

// The kill goes through the pinned process, never the PID file. A guest
// relaunched here under a new pid after the paused one exited is untouched.
func TestAStopNeverSignalsAProcessThePauseDidNotPin(t *testing.T) {
	m, instance, pid, _, _ := fencedGuest(t, "vol-pausedreuse")
	m.PauseForFence(t.Context(), "vol-pausedreuse", "lease unconfirmed, no peer", 7, 0)

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
// precondition leaves it paused for the same fence. A repeat for a fence
// already resumed succeeds without continuing it again.
func TestAResumeContinuesOnlyAfterItsPrecondition(t *testing.T) {
	m, instance, _, qmpCalls, _ := fencedGuest(t, "vol-pausedresume")
	m.PauseForFence(t.Context(), "vol-pausedresume", "lease unconfirmed, no peer", 7, 0)

	moved := errors.New("the instance is assigned elsewhere")
	_, resumed, err := m.ResumeAfterFence(t.Context(), "vol-pausedresume", 7, func(string) error { return moved })
	require.ErrorIs(t, err, moved)
	assert.False(t, resumed)
	assert.Zero(t, qmpCalls("cont"), "a resume whose precondition failed must not continue the guest")

	_, _, err = m.ResumeAfterFence(t.Context(), "vol-pausedresume", 8, nil)
	require.ErrorIs(t, err, ErrNotFencePaused, "a resume for another fence generation must not act")

	id, resumed, err := m.ResumeAfterFence(t.Context(), "vol-pausedresume", 7, func(string) error { return nil })
	require.NoError(t, err, "the failed precondition left the guest paused for the same fence")
	assert.True(t, resumed)
	assert.Equal(t, instance.ID, id)
	assert.Equal(t, 1, qmpCalls("cont"), "the guest is continued once its volume is back")

	_, resumed, err = m.ResumeAfterFence(t.Context(), "vol-pausedresume", 7, nil)
	require.NoError(t, err, "a repeated resume, as after a lost acknowledgement, must succeed")
	assert.False(t, resumed)
	assert.Equal(t, 1, qmpCalls("cont"), "a repeated resume must not continue the guest again")
}

// A stop for a fence the guest is not paused under is ignored, so a stale stop
// cannot reach a guest started again on this volume.
func TestAStaleFenceStopIsIgnored(t *testing.T) {
	m, _, pid, _, _ := fencedGuest(t, "vol-pausedstale")
	m.PauseForFence(t.Context(), "vol-pausedstale", "lease unconfirmed, no peer", 7, 0)

	assert.Empty(t, m.StopFencePaused(t.Context(), "vol-pausedstale", 6, "an older fence"))
	m.goroutineWg.Wait()
	assert.Truef(t, hostprocess.ProcessAlive(pid), "a stop for another fence killed the guest (pid %d)", pid)

	assert.NotEmpty(t, m.StopFencePaused(t.Context(), "vol-pausedstale", 7, "the window ended"))
	waitFenceTeardown(t, m)
	assert.Falsef(t, hostprocess.ProcessAlive(pid), "the stop for its own fence left the guest (pid %d)", pid)
}

// The daemon stops a paused guest itself once the window and its grace have
// passed, whether or not the provider is heard from again.
func TestAPausedGuestIsStoppedWhenTheDaemonsWindowEnds(t *testing.T) {
	m, _, pid, _, _ := fencedGuest(t, "vol-pausedexpiry")
	m.fenceGrace = time.Millisecond
	m.PauseForFence(t.Context(), "vol-pausedexpiry", "lease unconfirmed, no peer", 7, 50*time.Millisecond)

	require.Eventuallyf(t, func() bool { return !hostprocess.ProcessAlive(pid) }, 5*time.Second, 20*time.Millisecond,
		"the paused guest (pid %d) outlived the daemon's own window", pid)
}

// The heartbeat's I/O-error resume must leave a fence-paused guest alone: its
// export is gone until the fence resumes it.
func TestAFencePausedGuestIsNotResumedByTheIOErrorPath(t *testing.T) {
	m, instance, _, qmpCalls, _ := fencedGuest(t, "vol-pausedioerr")
	m.PauseForFence(t.Context(), "vol-pausedioerr", "lease unconfirmed, no peer", 7, 0)

	for range 3 {
		m.resumeAfterIOError(t.Context(), instance)
	}
	assert.Zero(t, qmpCalls("cont"), "a guest paused for a fence was continued with no disk behind it")
}

// A resume whose precondition fails leaves the fence in force, so the daemon's
// own expiry still stops the guest.
func TestAFailedResumeKeepsTheExpiry(t *testing.T) {
	m, _, pid, qmpCalls, _ := fencedGuest(t, "vol-pausedkeepexpiry")
	m.fenceGrace = time.Millisecond
	m.PauseForFence(t.Context(), "vol-pausedkeepexpiry", "lease unconfirmed, no peer", 7, 100*time.Millisecond)

	_, _, err := m.ResumeAfterFence(t.Context(), "vol-pausedkeepexpiry", 7, func(string) error { return errors.New("moved") })
	require.Error(t, err)
	require.Eventuallyf(t, func() bool { return !hostprocess.ProcessAlive(pid) }, 5*time.Second, 20*time.Millisecond,
		"a guest whose resume failed outlived its window (pid %d)", pid)
	assert.Zero(t, qmpCalls("cont"))
}

// A guest that lost two volumes continues only once both are back, and a stop
// for the one still fenced kills it even after the other was restored.
func TestAGuestWithTwoFencedVolumesWaitsForBoth(t *testing.T) {
	m, instance, pid, qmpCalls, _ := fencedGuest(t, "vol-pausedfirst")
	m.Inspect(instance, func(v *VM) {
		v.EBSRequests.Requests = append(v.EBSRequests.Requests, EBSRequest{Name: "vol-pausedsecond"})
	})
	m.PauseForFence(t.Context(), "vol-pausedfirst", "lease unconfirmed, no peer", 7, 0)
	m.PauseForFence(t.Context(), "vol-pausedsecond", "lease unconfirmed, no peer", 9, 0)

	_, resumed, err := m.ResumeAfterFence(t.Context(), "vol-pausedfirst", 7, nil)
	require.NoError(t, err)
	assert.False(t, resumed, "one volume back is not the guest back")
	assert.Zero(t, qmpCalls("cont"))

	assert.NotEmpty(t, m.StopFencePaused(t.Context(), "vol-pausedsecond", 9, "the window ended"))
	waitFenceTeardown(t, m)
	assert.Falsef(t, hostprocess.ProcessAlive(pid), "the guest (pid %d) outlived a stop for its other volume", pid)
}
