package volume

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/deadpid"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const leakGateNode = "node-home"

// failedKill is the record a failed QEMU kill leaves: QEMU and volumes failed.
func failedKill(id, node string) *vm.VM {
	return &vm.VM{
		ID: id, LastNode: node, Status: vm.StateTerminated,
		Teardown: map[string]string{vm.TeardownQEMU: string(vm.TeardownFailed), vm.TeardownVolumes: string(vm.TeardownFailed)},
	}
}

// sweepAfterFailedKill runs the real leak reaper over a seeded in-use volume
// using the leaked set the daemon derives from the terminated record.
func sweepAfterFailedKill(t *testing.T, rec *vm.VM) (state, attached string, orphaned bool) {
	t.Helper()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", objectstore.NewMemoryObjectStore())
	seedVolume(t, svc, "vol-leakgate", "in-use", rec.ID)
	reaper := svc.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return vm.LeakedVolumeInstances([]*vm.VM{rec}, leakGateNode), nil
	})
	_, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	meta, err := svc.GetVolumeMetadata(testVolAccountID, "vol-leakgate")
	require.NoError(t, err)
	return meta.State, meta.AttachedInstance, meta.Tags[orphanTagKey] != ""
}

func isolatedPidDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// stamped is the failed-kill record with QEMU teardown set to state.
func stamped(state vm.TeardownState) *vm.VM {
	rec := failedKill("i-leakgate", leakGateNode)
	rec.Teardown[vm.TeardownQEMU] = string(state)
	return rec
}

// TestS7_LeakReaperNeverFreesAVolumeWhileQEMUMayBeAlive enforces ADR-0003:S7: a
// failed kill leaves the volume in-use and attached until QEMU is proven gone.
func TestS7_LeakReaperNeverFreesAVolumeWhileQEMUMayBeAlive(t *testing.T) {
	for _, stamp := range []vm.TeardownState{vm.TeardownFailed, vm.TeardownDone} {
		t.Run("qemu teardown "+string(stamp), func(t *testing.T) { leakGateKeepsVolume(t, stamp) })
	}
}

func leakGateKeepsVolume(t *testing.T, stamp vm.TeardownState) {
	for _, content := range []string{"not-a-pid", "0", "-1"} {
		t.Run("pid file "+content, func(t *testing.T) {
			isolatedPidDir(t)
			path, err := hostprocess.GeneratePidFile("i-leakgate")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			state, attached, orphaned := sweepAfterFailedKill(t, stamped(stamp))
			assert.Equal(t, "in-use", state)
			assert.Equal(t, "i-leakgate", attached)
			assert.False(t, orphaned)
		})
	}

	t.Run("live process", func(t *testing.T) {
		isolatedPidDir(t)
		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		var wg sync.WaitGroup
		wg.Go(func() { _ = cmd.Wait() })
		t.Cleanup(func() { _ = cmd.Process.Kill(); wg.Wait() })
		require.NoError(t, hostprocess.WritePidFile("i-leakgate", cmd.Process.Pid))
		state, attached, orphaned := sweepAfterFailedKill(t, stamped(stamp))
		assert.Equal(t, "in-use", state)
		assert.Equal(t, "i-leakgate", attached)
		assert.False(t, orphaned)
	})

	t.Run("unsignalable process", func(t *testing.T) {
		isolatedPidDir(t)
		t.Cleanup(hostprocess.SetSignalProbe(func(int) error { return syscall.EPERM }))
		require.NoError(t, hostprocess.WritePidFile("i-leakgate", deadpid.PID))
		state, attached, orphaned := sweepAfterFailedKill(t, stamped(stamp))
		assert.Equal(t, "in-use", state)
		assert.Equal(t, "i-leakgate", attached)
		assert.False(t, orphaned)
	})
}

// TestLeakReaperFreesTheVolumeOnceQEMUIsProvenGone keeps the existing outcome
// for each proof of exit: the volume is tagged orphaned and made available.
func TestLeakReaperFreesTheVolumeOnceQEMUIsProvenGone(t *testing.T) {
	t.Run("dead process", func(t *testing.T) {
		isolatedPidDir(t)
		require.NoError(t, hostprocess.WritePidFile("i-leakgate", deadpid.PID))
		state, attached, orphaned := sweepAfterFailedKill(t, failedKill("i-leakgate", leakGateNode))
		assert.Equal(t, "available", state)
		assert.Empty(t, attached)
		assert.True(t, orphaned)
	})
	t.Run("absent pid file", func(t *testing.T) {
		isolatedPidDir(t)
		state, _, orphaned := sweepAfterFailedKill(t, failedKill("i-leakgate", leakGateNode))
		assert.Equal(t, "available", state)
		assert.True(t, orphaned)
	})
}

// TestS7_LeakedSetJudgesOnlyTheHomeNode enforces ADR-0003:S7: another node's
// record is never in the set and its PID file is never probed. A counting probe
// over a present PID file shows the home-node check runs before the QEMU one.
func TestS7_LeakedSetJudgesOnlyTheHomeNode(t *testing.T) {
	isolatedPidDir(t)
	var probes int
	t.Cleanup(hostprocess.SetSignalProbe(func(int) error { probes++; return syscall.ESRCH }))
	require.NoError(t, hostprocess.WritePidFile("i-other", deadpid.PID))
	require.NoError(t, hostprocess.WritePidFile("i-empty", deadpid.PID))

	leaked := vm.LeakedVolumeInstances([]*vm.VM{failedKill("i-other", "node-other"), failedKill("i-empty", "")}, leakGateNode)
	assert.Empty(t, leaked)
	assert.Zero(t, probes, "a PID file was judged on a node that does not own the instance")
}

func mustPidPath(t *testing.T, id string) string {
	t.Helper()
	path, err := hostprocess.GeneratePidFile(id)
	require.NoError(t, err)
	return path
}
