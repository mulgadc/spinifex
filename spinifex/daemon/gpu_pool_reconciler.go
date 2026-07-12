package daemon

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// gpuPoolLiveStates are the instance states that legitimately still hold a
// GPU pool entry: mid-launch, running, or mid-teardown (stopCleanup /
// terminateCleanup release the slot before the instance settles into
// Stopped/Terminated). Anything else — Stopped, Error, Terminated, or the
// instance being entirely absent from the running map — means the slot
// should already have been released and was not.
var gpuPoolLiveStates = map[vm.InstanceState]bool{
	vm.StateProvisioning: true,
	vm.StatePending:      true,
	vm.StateRunning:      true,
	vm.StateStopping:     true,
	vm.StateShuttingDown: true,
}

// gpuPoolReconciler frees GPU pool entries whose owning instance is absent
// or has settled in a terminal, non-running state. It is the backstop for
// the synchronous release in vm/crash_recovery.go: a node-down mid-cascade,
// or any other gap that skips the synchronous release, would otherwise leak
// the slot forever, since nothing else reclaims it. Node-local vm.Reaper run
// by the GarbageCollector backstop, mirroring eniReconciler.
type gpuPoolReconciler struct {
	d *Daemon
}

var _ vm.Reaper = (*gpuPoolReconciler)(nil)

// newGPUPoolReconciler builds the reconciler over the daemon. Returns nil
// when the VM manager is absent so the caller can skip wiring it. The GPU
// manager itself is read live from d.gpuManager on every sweep (not
// captured here) since SIGHUP can replace it at runtime.
func (d *Daemon) newGPUPoolReconciler() *gpuPoolReconciler {
	if d.vmMgr == nil {
		return nil
	}
	return &gpuPoolReconciler{d: d}
}

func (r *gpuPoolReconciler) Class() string         { return "gpu-pool" }
func (r *gpuPoolReconciler) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep walks the GPU pool and releases any claimed entry whose instance is
// absent from the running map or has settled in a non-running state.
func (r *gpuPoolReconciler) Sweep(ctx context.Context) (int, error) {
	mgr := r.d.gpuManager
	if mgr == nil {
		return 0, nil
	}

	reaped := 0
	for _, entry := range mgr.Snapshot() {
		if ctx.Err() != nil {
			return reaped, ctx.Err()
		}
		if entry.InstanceID == "" || r.instanceHoldsSlot(entry.InstanceID) {
			continue
		}
		slog.Warn("gpu-reconciler: releasing orphaned GPU pool entry",
			"instanceId", entry.InstanceID, "pci", entry.Device.PCIAddress)
		if err := mgr.Release(entry.InstanceID); err != nil {
			slog.Warn("gpu-reconciler: release failed", "instanceId", entry.InstanceID, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// instanceHoldsSlot reports whether instanceID names a VM in a state that
// legitimately still holds its GPU pool entry.
func (r *gpuPoolReconciler) instanceHoldsSlot(instanceID string) bool {
	instance, ok := r.d.vmMgr.Get(instanceID)
	if !ok {
		return false
	}
	return gpuPoolLiveStates[r.d.vmMgr.Status(instance)]
}
