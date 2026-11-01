package vm

//test:in-package — the fence teardown, the I/O-error resume and the QMP client
//they drive are unexported manager behaviour with no exported surface.

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fencedGuestTeardownBound is how long a fenced guest may take to go. Well
// under pidFileRemovalTimeout, so a teardown that tries a graceful shutdown first
// cannot meet it.
const fencedGuestTeardownBound = 5 * time.Second

// fencedGuest is a running instance with a live stand-in QEMU behind its PID
// file and a QMP monitor that answers, using volumeID.
func fencedGuest(t *testing.T, volumeID string) (*Manager, *VM, int, func(string) int, *recordingInstanceCleaner) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)

	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	var reaped sync.WaitGroup
	reaped.Go(func() { _ = cmd.Wait() })
	t.Cleanup(func() { _ = cmd.Process.Kill(); reaped.Wait() })

	const id = "i-fencedguest"
	require.NoError(t, hostprocess.WritePidFile(id, cmd.Process.Pid))

	sockPath, count := startRecordingQMPListener(t)
	client, err := qmp.NewQMPClient(sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Conn.Close() })

	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{NodeID: "node-a", InstanceCleaner: cleaner})
	instance := &VM{
		ID:          id,
		Status:      StateRunning,
		Instance:    &ec2.Instance{},
		QMPClient:   client,
		EBSRequests: EBSRequests{Requests: []EBSRequest{{Name: volumeID}}},
	}
	m.Insert(instance)
	return m, instance, cmd.Process.Pid, count, cleaner
}

// waitFenceTeardown waits for FenceVolume's teardown goroutine, failing past
// fencedGuestTeardownBound.
func waitFenceTeardown(t *testing.T, m *Manager) time.Duration {
	t.Helper()
	began := time.Now()
	finished := make(chan struct{})
	go func() { m.goroutineWg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(fencedGuestTeardownBound):
		t.Fatalf("ADR-0007:S4: the fenced guest's teardown was still running after %s; "+
			"a fenced guest has lost its disk, and while it lingers a survivor can start the instance again. "+
			"Fix: kill it in FenceVolume (fence.go) instead of shutting it down gracefully", fencedGuestTeardownBound)
	}
	return time.Since(began)
}

// TestS4_FencedGuestIsKilledWithoutAGracefulShutdown enforces ADR-0007:S4, "a
// fence that the former node's realization path respects". The guest's export is
// already gone, so an ACPI powerdown can flush nothing, and the seconds it spends
// trying are seconds the guest runs on this node while a survivor starts it.
func TestS4_FencedGuestIsKilledWithoutAGracefulShutdown(t *testing.T) {
	m, instance, pid, qmpCalls, _ := fencedGuest(t, "vol-fencedkill")

	m.FenceVolume(t.Context(), "vol-fencedkill", "lease moved to node-b")
	took := waitFenceTeardown(t, m)

	assert.Falsef(t, hostprocess.ProcessAlive(pid),
		"ADR-0007:S4: the fenced guest's QEMU (pid %d) survived its teardown", pid)
	assert.Zero(t, qmpCalls("system_powerdown"),
		"ADR-0007:S4: a fenced guest has no disk to flush, so it must not be asked to power down")
	_, perr := hostprocess.ReadPidFile(instance.ID)
	assert.Error(t, perr, "a SIGKILLed QEMU never removes its own PID file, so the fence must")
	t.Logf("fenced guest torn down in %s", took)
}

// TestS7_FenceStopsButNeverDestroysTheInstance enforces ADR-0003:S7 on the fast
// teardown: the fence is an automatic path, so the instance is left stopped with
// its volumes, never terminated or stripped of them.
func TestS7_FenceStopsButNeverDestroysTheInstance(t *testing.T) {
	m, instance, _, _, cleaner := fencedGuest(t, "vol-fencedkeep")

	m.FenceVolume(t.Context(), "vol-fencedkeep", "lease moved to node-b")
	waitFenceTeardown(t, m)

	assert.Equal(t, StateError, m.Status(instance),
		"ADR-0003:S7: a fenced instance must be left stopped, not terminated")
	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	assert.Empty(t, cleaner.deleteVolumes, "ADR-0003:S7: a fence must never delete the instance's volumes")
	assert.Empty(t, cleaner.detachAndDeleteENI, "ADR-0003:S7: a fence must never delete the instance's interfaces")
	assert.Len(t, instance.EBSRequests.Requests, 1, "ADR-0003:S7: the fenced instance keeps its volume attachments")
}

// TestS4_FencedGuestIsNeverResumed enforces ADR-0007:S4 on the I/O-error path. A
// fenced guest is paused on the write its dead export refused, and the heartbeat
// may have read it running a moment before the fence landed.
func TestS4_FencedGuestIsNeverResumed(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m, instance, qmpCalls := pausedGuest(t)
	m.Inspect(instance, func(v *VM) {
		v.Instance = &ec2.Instance{}
		v.EBSRequests = EBSRequests{Requests: []EBSRequest{{Name: "vol-fencedpaused"}}}
	})

	m.FenceVolume(t.Context(), "vol-fencedpaused", "lease moved to node-b")
	m.goroutineWg.Wait()

	for range 3 {
		m.resumeAfterIOError(t.Context(), instance)
	}
	assert.Zero(t, qmpCalls("cont"),
		"ADR-0007:S4: a guest whose volume is fenced must never be resumed. "+
			"Fix: check the fence in resumeAfterIOError (lifecycle.go) before sending cont")
}
