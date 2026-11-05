//test:in-package — drives the ordinary terminate path over the shared manager
//and state-store doubles.

package vm

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/deadpid"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortenPidFileRemovalWait(t *testing.T) {
	t.Helper()
	orig := pidFileRemovalTimeout
	pidFileRemovalTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pidFileRemovalTimeout = orig })
}

func terminateGateInstance(id string) *VM {
	v := runningUEFIInstance(id)
	v.LastNode = "test-node"
	return v
}

// TestS7_OrdinaryTerminateNeverDeletesVolumesWhileQEMUMayBeAlive enforces
// ADR-0003:S7: when terminate cannot prove QEMU gone, it deletes and detaches no
// volume, removes no UEFI store and does not stamp QEMU done. The teardown reaper
// completes the work once exit is proven.
func TestS7_OrdinaryTerminateNeverDeletesVolumesWhileQEMUMayBeAlive(t *testing.T) {
	shortenPidFileRemovalWait(t)
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)

	cases := map[string]func(t *testing.T, id string){
		"corrupt pid file":   func(t *testing.T, id string) { writeCorruptPidFile(t, id, "not-a-pid") },
		"zero pid":           func(t *testing.T, id string) { writeCorruptPidFile(t, id, "0") },
		"negative pid":       func(t *testing.T, id string) { writeCorruptPidFile(t, id, "-1") },
		"unsignalable pid": func(t *testing.T, id string) {
			t.Cleanup(hostprocess.SetSignalProbe(func(int) error { return syscall.EPERM }))
			require.NoError(t, hostprocess.WritePidFile(id, deadpid.PID))
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStateStore()
			m, cleaner, _, _, _ := terminateTestManager(t, store)
			uefi := &fakeUEFIStores{has: true}
			withUEFIStores(m, uefi)
			const id = "i-terminate-unproven"
			seed(t, id)
			m.InsertIfAbsent(terminateGateInstance(id))

			require.NoError(t, m.Terminate(id))

			assert.Empty(t, cleaner.deleteVolumes, "no volume may be deleted or detached")
			marked, removed := uefi.calls()
			assert.Empty(t, marked)
			assert.Empty(t, removed)
			teardown := store.terminated[id].Teardown
			assert.Equal(t, string(TeardownFailed), teardown[TeardownQEMU])
			assert.Equal(t, string(TeardownFailed), teardown[TeardownVolumes])
			assert.Equal(t, string(TeardownFailed), teardown[TeardownUEFI])

			// Once exit is proven the teardown reaper finishes what was held back.
			require.NoError(t, os.Remove(mustPidFilePath(t, id)))
			_, err := m.NewTerminatedTeardownReaper().Sweep(context.Background())
			require.NoError(t, err)
			assert.Equal(t, []string{id}, cleaner.deleteVolumes)
			assert.Equal(t, string(TeardownDone), store.terminated[id].Teardown[TeardownQEMU])
		})
	}
}

// TestTerminateWithQEMUGoneCleansVolumesAndUEFIAsBefore keeps the successful
// terminate unchanged: QEMU done, volumes deleted, UEFI store recorded.
func TestTerminateWithQEMUGoneCleansVolumesAndUEFIAsBefore(t *testing.T) {
	shortenPidFileRemovalWait(t)
	store := newFakeStateStore()
	m, cleaner, _, _, _ := terminateTestManager(t, store)
	uefi := &fakeUEFIStores{has: true}
	withUEFIStores(m, uefi)
	const id = "i-terminate-proven"
	m.InsertIfAbsent(terminateGateInstance(id))

	require.NoError(t, m.Terminate(id))

	assert.Equal(t, []string{id}, cleaner.deleteVolumes)
	marked, _ := uefi.calls()
	assert.Equal(t, []string{id}, marked)
	teardown := store.terminated[id].Teardown
	assert.Equal(t, string(TeardownDone), teardown[TeardownQEMU])
	assert.Equal(t, string(TeardownDone), teardown[TeardownVolumes])
}

func mustPidFilePath(t *testing.T, id string) string {
	t.Helper()
	path, err := hostprocess.GeneratePidFile(id)
	require.NoError(t, err)
	return path
}
