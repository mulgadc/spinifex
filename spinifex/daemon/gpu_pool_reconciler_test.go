package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimMIGSliceForReconcilerTest seeds a one-slice MIG pool and claims it for
// instanceID. Uses the MIG path (not whole-GPU) so the test never touches
// real VFIO/sysfs state — MIG release is pure in-memory bookkeeping.
func claimMIGSliceForReconcilerTest(t *testing.T, instanceID string) *gpu.Manager {
	t.Helper()
	mdevPath := filepath.Join(t.TempDir(), "mdev0")
	require.NoError(t, os.WriteFile(mdevPath, []byte("x"), 0o600))

	mgr := gpu.NewManager(nil)
	mgr.AddMIGInstances(gpu.GPUDevice{PCIAddress: "0000:03:00.0"}, []gpu.MIGInstance{
		{Profile: gpu.MIGProfile{Name: "1g.10gb"}, MdevPath: mdevPath},
	})
	_, _, err := mgr.Claim(instanceID, "1g.10gb")
	require.NoError(t, err)
	require.Equal(t, 0, mgr.Available(), "GPU slot must be claimed before the sweep")
	return mgr
}

// TestGPUPoolReconciler_FreesOrphanedSlot is the regression test for
// mulga-keptb: a GPU pool entry whose InstanceID names an instance absent
// from the running map entirely must be released.
func TestGPUPoolReconciler_FreesOrphanedSlot(t *testing.T) {
	mgr := claimMIGSliceForReconcilerTest(t, "i-orphan")
	d := &Daemon{vmMgr: vm.NewManager(), gpuManager: mgr}
	r := d.newGPUPoolReconciler()
	require.NotNil(t, r)

	reaped, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, 1, mgr.Available(), "orphaned slot must return to the pool")
}

// TestGPUPoolReconciler_LeavesRunningInstanceAlone verifies the reconciler
// does not touch a slot whose instance is actively running.
func TestGPUPoolReconciler_LeavesRunningInstanceAlone(t *testing.T) {
	mgr := claimMIGSliceForReconcilerTest(t, "i-running")
	vmMgr := vm.NewManager()
	vmMgr.Insert(&vm.VM{ID: "i-running", Status: vm.StateRunning})
	d := &Daemon{vmMgr: vmMgr, gpuManager: mgr}
	r := d.newGPUPoolReconciler()
	require.NotNil(t, r)

	reaped, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 0, reaped)
	assert.Equal(t, 0, mgr.Available(), "a running instance's GPU slot must not be reclaimed")
}

// TestGPUPoolReconciler_FreesSlotForTerminalInstance covers the backstop case
// for a stuck StateError instance still holding a GPU slot — the same
// scenario the synchronous crash-recovery release targets directly, here
// caught by the reconciler as a second line of defense.
func TestGPUPoolReconciler_FreesSlotForTerminalInstance(t *testing.T) {
	mgr := claimMIGSliceForReconcilerTest(t, "i-error")
	vmMgr := vm.NewManager()
	vmMgr.Insert(&vm.VM{ID: "i-error", Status: vm.StateError})
	d := &Daemon{vmMgr: vmMgr, gpuManager: mgr}
	r := d.newGPUPoolReconciler()
	require.NotNil(t, r)

	reaped, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	assert.Equal(t, 1, mgr.Available())
}

// TestGPUPoolReconciler_NoGPUManager_NoOp verifies the reconciler is a
// harmless no-op when GPU passthrough is disabled.
func TestGPUPoolReconciler_NoGPUManager_NoOp(t *testing.T) {
	d := &Daemon{vmMgr: vm.NewManager()}
	r := d.newGPUPoolReconciler()
	require.NotNil(t, r)

	reaped, err := r.Sweep(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 0, reaped)
}

// TestNewGPUPoolReconciler_NilWithoutVMManager verifies the constructor
// refuses to build a reconciler with no VM manager to query.
func TestNewGPUPoolReconciler_NilWithoutVMManager(t *testing.T) {
	d := &Daemon{}
	assert.Nil(t, d.newGPUPoolReconciler())
}
