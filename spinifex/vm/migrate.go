package vm

import "log/slog"

// MigrateStoppedToSharedKV writes instance to the "stopped" KV bucket and
// removes it from the local map. Returns true only when both the write and
// DeleteIf succeeded — a concurrent slot reclaim reports false to prevent
// firing OnInstanceDown against a different live instance at the same id.
func (m *Manager) MigrateStoppedToSharedKV(instance *VM) bool {
	if m.deps.StateStore == nil {
		return false
	}
	return m.migrateInstanceToKV(instance, m.deps.StateStore.WriteStoppedInstance, "stopped")
}

// MigrateTerminatedToKV writes instance to the cluster-shared "terminated"
// KV bucket and removes it from the local running map. Same semantics as
// MigrateStoppedToSharedKV but for the terminated bucket.
func (m *Manager) MigrateTerminatedToKV(instance *VM) bool {
	if m.deps.StateStore == nil {
		return false
	}
	return m.migrateInstanceToKV(instance, m.deps.StateStore.WriteTerminatedInstance, "terminated")
}

// migrateInstanceToKV is the shared body of MigrateStoppedToSharedKV and
// MigrateTerminatedToKV.
func (m *Manager) migrateInstanceToKV(instance *VM, writeFn func(string, *VM) error, label string) bool {
	instance.LastNode = m.deps.NodeID
	if err := writeFn(instance.ID, instance); err != nil {
		slog.Error("Failed to migrate instance to KV",
			"instance", instance.ID, "bucket", label, "err", err)
		return false
	}
	if !m.DeleteIf(instance.ID, instance) {
		slog.Info("Slot reclaimed by another handler during migration; skipping local delete",
			"instance", instance.ID, "bucket", label)
		return false
	}
	slog.Info("Migrated instance to KV", "instance", instance.ID, "bucket", label)
	return true
}
