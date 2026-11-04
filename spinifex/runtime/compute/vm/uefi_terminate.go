package vm

import "log/slog"

// UEFIStoreTerminator settles a terminated instance's UEFI variable store. The
// objects are deleted by a cluster-wide reaper, so terminate only records the
// obligation and removes this node's local copy.
type UEFIStoreTerminator interface {
	// MarkUEFIStoreDeleting moves the instance's store record to deleting. It
	// reports false with no error when the instance has no store.
	MarkUEFIStoreDeleting(instanceID string) (bool, error)
	// RemoveLocalUEFIStore removes this node's copy of the store. Idempotent.
	RemoveLocalUEFIStore(instanceID string) error
}

// markUEFIStoreForDeletion records a terminating instance's store for the
// reaper. A failed mark is stamped failed, which the reaper completes from the
// terminated record; an instance with no store gets no teardown entry.
func (m *Manager) markUEFIStoreForDeletion(instance *VM) {
	if m.deps.UEFIStores == nil {
		return
	}
	has, err := m.deps.UEFIStores.MarkUEFIStoreDeleting(instance.ID)
	switch {
	case err != nil:
		slog.Warn("Failed to record UEFI variable store for deletion; the store reaper completes it",
			"instanceId", instance.ID, "err", err)
		m.markTeardown(instance, TeardownUEFI, TeardownFailed)
	case has:
		m.markTeardown(instance, TeardownUEFI, TeardownPending)
	}
	if err := m.deps.UEFIStores.RemoveLocalUEFIStore(instance.ID); err != nil {
		slog.Warn("Failed to remove local UEFI variable store", "instanceId", instance.ID, "err", err)
	}
}
