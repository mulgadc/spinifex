//test:in-package — replaces the unexported QEMU kill and drives both reapers
//over the shared manager and state-store doubles.

package vm

import (
	"context"
	"errors"
	"io/fs"
	"os"
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

// unknownPidContents are PID file contents that are not proof of a dead process.
var unknownPidContents = []string{"not-a-pid", "0", "-1"}

func writeCorruptPidFile(t *testing.T, id, content string) {
	t.Helper()
	path, err := hostprocess.GeneratePidFile(id)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// TestS7_UnknownQEMUStateNeverPermitsVolumeDeletion enforces ADR-0003:S7: a PID
// file that cannot be read is unknown state, not proof QEMU exited, on both the
// backstop and the teardown reaper.
func TestS7_UnknownQEMUStateNeverPermitsVolumeDeletion(t *testing.T) {
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)

	for _, content := range unknownPidContents {
		t.Run(content, func(t *testing.T) {
			t.Run("stuck terminate backstop", func(t *testing.T) {
				cleaner := &recordingInstanceCleaner{}
				stuck, store := newStuckTerminateReaper(t, cleaner)
				const id = "i-corrupt-pid-stuck"
				writeCorruptPidFile(t, id, content)
				stuck.m.InsertIfAbsent(&VM{
					ID:             id,
					Status:         StateShuttingDown,
					ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
				})
				_, err := stuck.Sweep(context.Background())
				require.NoError(t, err)
				assert.Empty(t, cleaner.deleteVolumes)
				assert.Equal(t, string(TeardownFailed), store.terminated[id].Teardown[TeardownQEMU])
			})

			t.Run("teardown reaper", func(t *testing.T) {
				cleaner := &recordingInstanceCleaner{}
				stuck, store := newStuckTerminateReaper(t, cleaner)
				const id = "i-corrupt-pid-reaper"
				writeCorruptPidFile(t, id, content)
				require.NoError(t, store.WriteTerminatedInstance(id, &VM{
					ID:       id,
					Status:   StateTerminated,
					LastNode: "test-node",
					Teardown: map[string]string{TeardownQEMU: string(TeardownFailed), TeardownVolumes: string(TeardownFailed)},
				}))
				_, err := stuck.m.NewTerminatedTeardownReaper().Sweep(context.Background())
				require.NoError(t, err)
				assert.Empty(t, cleaner.deleteVolumes)
				assert.Equal(t, string(TeardownFailed), store.terminated[id].Teardown[TeardownQEMU])
			})
		})
	}
}

// TestStuckTerminateKillFailureKeepsTheUEFIObligation checks the failed-kill
// handoff still records the UEFI store for the store reaper to complete.
func TestStuckTerminateKillFailureKeepsTheUEFIObligation(t *testing.T) {
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	orig := forceKillQEMU
	forceKillQEMU = func(int, time.Duration) error { return errors.New("kill failed") }
	t.Cleanup(func() { forceKillQEMU = orig })

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	m := NewManagerWithDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		InstanceCleaner: &recordingInstanceCleaner{},
		UEFIStores:      &fakeUEFIStores{},
	})
	const id = "i-kill-fails-uefi"
	liveQEMU(t, id)
	m.InsertIfAbsent(&VM{
		ID:             id,
		Status:         StateShuttingDown,
		ShuttingDownAt: time.Now().Add(-(stuckTerminateTimeout + time.Minute)),
	})
	_, err := m.NewStuckTerminateReaper().Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, string(TeardownFailed), store.terminated[id].Teardown[TeardownUEFI],
		"the UEFI obligation must survive the failed-kill handoff")
}

// TestS7_TeardownReaperDoesNotJudgeQEMUOnAnotherNode checks a record with no
// recorded node keeps its volumes while QEMU teardown is outstanding: a node
// that never ran the guest finds no PID file, which is not proof of exit.
func TestS7_TeardownReaperDoesNotJudgeQEMUOnAnotherNode(t *testing.T) {
	cleaner := &recordingInstanceCleaner{}
	stuck, store := newStuckTerminateReaper(t, cleaner)
	const id = "i-no-last-node"
	require.NoError(t, store.WriteTerminatedInstance(id, &VM{
		ID:       id,
		Status:   StateTerminated,
		Teardown: map[string]string{TeardownQEMU: string(TeardownFailed), TeardownVolumes: string(TeardownFailed)},
	}))
	_, err := stuck.m.NewTerminatedTeardownReaper().Sweep(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cleaner.deleteVolumes)
	assert.Equal(t, string(TeardownFailed), store.terminated[id].Teardown[TeardownQEMU])
}

// TestS7_CorruptPidFileSurvivesTheOrphanReaperAndKeepsVolumes enforces
// ADR-0003:S7 across reapers: the orphan reaper must not turn an unreadable PID
// file into a missing one, which the teardown reaper reads as proof of exit.
func TestS7_CorruptPidFileSurvivesTheOrphanReaperAndKeepsVolumes(t *testing.T) {
	for _, content := range unknownPidContents {
		t.Run(content, func(t *testing.T) {
			corruptPidFileSurvivesTheOrphanReaper(t, content)
		})
	}
}

func corruptPidFileSurvivesTheOrphanReaper(t *testing.T, content string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{NodeID: "test-node", StateStore: store, InstanceCleaner: cleaner})
	const id = "i-corrupt-pid-orphan"
	writeCorruptPidFile(t, id, content)
	m.InsertIfAbsent(&VM{
		ID:             id,
		Status:         StateShuttingDown,
		LastNode:       "test-node",
		ShuttingDownAt: time.Now().Add(-vanishedQEMUGrace - time.Minute),
		Teardown:       map[string]string{TeardownQEMU: string(TeardownFailed), TeardownVolumes: string(TeardownFailed)},
	})

	reaped, err := m.NewOrphanQEMUReaper().Sweep(context.Background())
	require.NoError(t, err)
	assert.Zero(t, reaped)
	_, perr := hostprocess.ReadPidFile(id)
	assert.NotErrorIs(t, perr, fs.ErrNotExist, "the unreadable PID file must not be removed")
	assert.NotContains(t, store.terminated, id, "unknown QEMU state must not finalize the instance")

	_, err = m.NewTerminatedTeardownReaper().Sweep(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cleaner.deleteVolumes)
}

// TestTeardownReaperRemovesTheLocalUEFIStoreOnceQEMUIsGone checks the failed-kill
// handoff's local store copy is removed after proven exit, not left behind.
func TestTeardownReaperRemovesTheLocalUEFIStoreOnceQEMUIsGone(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	uefi := &fakeUEFIStores{}
	m := NewManagerWithDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		InstanceCleaner: &recordingInstanceCleaner{},
		UEFIStores:      uefi,
	})
	const id = "i-uefi-local"
	require.NoError(t, store.WriteTerminatedInstance(id, &VM{
		ID:       id,
		Status:   StateTerminated,
		LastNode: "test-node",
		Teardown: map[string]string{TeardownQEMU: string(TeardownFailed), TeardownUEFI: string(TeardownFailed)},
	}))
	_, err := m.NewTerminatedTeardownReaper().Sweep(context.Background())
	require.NoError(t, err)
	_, removed := uefi.calls()
	assert.Equal(t, []string{id}, removed)
	assert.Equal(t, string(TeardownDone), store.terminated[id].Teardown[TeardownQEMU])
}
