package vm

import "log/slog"

// HandleQEMUExit decides what a finished QEMU process means once startup was
// confirmed. A wait error is a crash. A clean exit after the heartbeat saw the
// guest power itself off is a customer stop, kept in the stopped bucket so
// StartInstances can bring it back. Any other clean exit is left to the
// heartbeat's crash detection.
func (m *Manager) HandleQEMUExit(instance *VM, waitErr error) {
	if waitErr != nil {
		if m.deps.CrashHandler != nil {
			m.deps.CrashHandler(instance, waitErr)
		}
		return
	}
	if !instance.guestPoweroff.Load() {
		slog.Info("VM process exited cleanly", "instance", instance.ID)
		return
	}

	slog.Info("Guest powered itself off, stopping instance", "instance", instance.ID)
	m.UpdateState(instance.ID, func(v *VM) { v.DesiredState = DesiredStopped })
	if err := m.Stop(instance.ID); err != nil {
		slog.Error("Stop after guest poweroff failed", "instance", instance.ID, "err", err)
	}
}
