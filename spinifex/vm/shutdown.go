package vm

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// pidFileRemovalTimeout is how long Stop/Terminate wait for QEMU's PID file
// to disappear after a graceful system_powerdown before resorting to
// SIGKILL. 20s is enough for an ACPI shutdown — guests that haven't
// responded by then won't.
const pidFileRemovalTimeout = 20 * time.Second

// Stop transitions a running instance to stopped: graceful QMP shutdown,
// volume unmount, tap teardown, resource deallocation. Persists to the
// cluster-shared "stopped" KV bucket and removes the instance from the
// local running map. Fires OnInstanceDown.
//
// Returns ErrInstanceNotFound if id is unknown, ErrInvalidTransition if
// the instance is not in a state that admits Stopping. Persistence errors
// after the in-memory transition succeeded are logged but not surfaced.
func (m *Manager) Stop(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}

	if err := m.transitionWithPrecheck(instance, StateStopping); err != nil {
		return err
	}

	m.stopCleanup(instance)

	m.UpdateState(instance.ID, func(v *VM) { v.LastNode = m.deps.NodeID })

	if err := m.transitionWithPrecheck(instance, StateStopped); err != nil {
		slog.Error("Failed to transition to stopped", "instanceId", instance.ID, "err", err)
	}

	if !m.MigrateStoppedToSharedKV(instance) {
		// Either StateStore unavailable / write failed (instance stays in
		// local map; restoreInstances retries on next boot) OR a concurrent
		// handler reclaimed the slot (id now resolves to a different live
		// VM). Either way, do not fire OnInstanceDown — firing it would
		// unsubscribe the per-id NATS subscriptions of the reclaimed
		// instance.
		return nil
	}

	if m.deps.Hooks.OnInstanceDown != nil {
		m.deps.Hooks.OnInstanceDown(instance.ID)
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state after stop, re-adding to local map for consistency",
			"instanceId", instance.ID, "err", err)
		m.InsertIfAbsent(instance)
		return nil
	}
	slog.Info("Released instance ownership to KV",
		"instanceId", instance.ID, "state", string(StateStopped), "lastNode", m.deps.NodeID)
	return nil
}

// StopAll fans Stop's per-instance shutdown out across every VM the
// manager currently holds. Used by the coordinated shutdown DRAIN phase
// and by the SIGTERM signal handler. Each instance shuts down in its own
// goroutine so total wall-time is bounded by the slowest VM; errors are
// logged per instance and never abort the fan-out.
//
// Volume + tap teardown only — instances persist locally so the cluster
// sees them as stopped after the daemon restarts. AWS resources (ENI,
// public IP, placement group) are not released because the instance is
// not being terminated.
func (m *Manager) StopAll() error {
	snapshot := m.Snapshot()
	if len(snapshot) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	for _, instance := range snapshot {
		wg.Add(1)
		go func(v *VM) {
			defer wg.Done()
			m.stopCleanup(v)
		}(instance)
	}
	wg.Wait()
	return nil
}

// Terminate transitions an instance to terminated: graceful shutdown,
// volume + ENI + IP cleanup, placement group removal. Persists to the
// cluster-shared "terminated" KV bucket and removes the instance from
// the local running map. Fires OnInstanceDown.
//
// Idempotent on already-shutting-down (the failed-launch goroutine is
// already cleaning up). Returns ErrInstanceNotFound if id is unknown,
// ErrInvalidTransition if the current state does not permit termination.
func (m *Manager) Terminate(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}

	if current := m.Status(instance); current == StateShuttingDown {
		// Concurrent failed-launch goroutine already owns cleanup.
		return nil
	}

	if err := m.transitionWithPrecheck(instance, StateShuttingDown); err != nil {
		return err
	}

	m.terminateCleanup(instance)

	return m.finalizeTerminated(instance)
}

// MarkFailed sets a failure reason, transitions to shutting-down
// synchronously, then runs the cleanup chain in a goroutine. Used when a
// launch errors mid-way: callers (NATS RunInstances handler, recovery
// worker, pending watchdog, system-instance launcher) get back control
// immediately and do not block on volume unmount, ENI delete, or KV
// writes.
//
// Tolerates instances already in a cleanup state (no-op) and instances
// that may or may not be present in the running-VM map.
func (m *Manager) MarkFailed(instance *VM, reason string) {
	skip := false
	var observed InstanceState
	m.Inspect(instance, func(v *VM) {
		observed = v.Status
		if v.Status == StateShuttingDown || v.Status == StateTerminated {
			skip = true
			return
		}
		if v.Instance != nil {
			v.Instance.StateReason = &ec2.StateReason{
				Code:    aws.String("Server.InternalError"),
				Message: aws.String(reason),
			}
		}
	})
	if skip {
		slog.Info("MarkFailed: instance already in cleanup state, skipping",
			"instanceId", instance.ID, "status", string(observed), "reason", reason)
		return
	}

	if err := m.transitionWithPrecheck(instance, StateShuttingDown); err != nil {
		slog.Error("MarkFailed transition failed", "instanceId", instance.ID, "err", err)
		// If this was a persistence-only failure, in-memory state is now
		// shutting-down and we still want to finalize. Otherwise bail.
		if m.Status(instance) != StateShuttingDown {
			return
		}
	}
	slog.Info("Instance marked as failed", "instanceId", instance.ID, "reason", reason)

	go func() {
		m.terminateCleanup(instance)
		if err := m.finalizeTerminated(instance); err != nil {
			slog.Error("MarkFailed finalize failed", "instanceId", instance.ID, "err", err)
		}
	}()
}

// finalizeTerminated transitions instance to terminated, writes the
// terminated KV entry, removes the instance from the local map, fires
// OnInstanceDown, and persists the running set. Shared by Terminate and
// MarkFailed.
func (m *Manager) finalizeTerminated(instance *VM) error {
	// Inspect (not UpdateState): MarkFailed may invoke this for an
	// instance that was never inserted into the local map.
	m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })

	if err := m.transitionWithPrecheck(instance, StateTerminated); err != nil {
		return fmt.Errorf("transition to terminated: %w", err)
	}

	if m.deps.StateStore != nil {
		if err := m.deps.StateStore.WriteTerminatedInstance(instance.ID, instance); err != nil {
			slog.Error("Failed to write terminated instance to KV, keeping in local state for retry",
				"instanceId", instance.ID, "err", err)
			return err
		}
	}

	if !m.DeleteIf(instance.ID, instance) {
		slog.Info("Instance was reclaimed by another handler, skipping local cleanup",
			"instanceId", instance.ID, "state", string(StateTerminated))
		return nil
	}

	if m.deps.Hooks.OnInstanceDown != nil {
		m.deps.Hooks.OnInstanceDown(instance.ID)
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state after terminate, re-adding to local map",
			"instanceId", instance.ID, "err", err)
		m.InsertIfAbsent(instance)
		return nil
	}
	slog.Info("Released instance ownership to KV",
		"instanceId", instance.ID, "state", string(StateTerminated), "lastNode", m.deps.NodeID)
	return nil
}

// stopCleanup performs the per-instance teardown shared by Stop and the
// initial section of Terminate: graceful QMP shutdown, PID-file wait,
// volume unmount, tap teardown (main + extra ENI + mgmt), resource
// deallocation. Per-step errors are logged and tolerated.
func (m *Manager) stopCleanup(instance *VM) {
	m.shutdownAndUnmount(instance)
	m.cleanupTapDevices(instance)
	if m.deps.InstanceCleaner != nil {
		m.deps.InstanceCleaner.ReleaseGPU(instance)
	}
	m.deallocateResources(instance)
}

// terminateCleanup is stopCleanup plus the AWS-resource cleanup that
// only applies on terminate: volume deletion, public IP release, ENI
// deletion, placement-group removal.
func (m *Manager) terminateCleanup(instance *VM) {
	m.shutdownAndUnmount(instance)

	if m.deps.InstanceCleaner != nil {
		m.deps.InstanceCleaner.DeleteVolumes(instance)
	}

	m.cleanupTapDevices(instance)

	if m.deps.InstanceCleaner != nil {
		m.deps.InstanceCleaner.ReleaseGPU(instance)
		m.deps.InstanceCleaner.ReleasePublicIP(instance)
		m.deps.InstanceCleaner.DetachAndDeleteENI(instance)
		m.deps.InstanceCleaner.RemoveFromPlacementGroup(instance)
	}

	m.deallocateResources(instance)
}

// shutdownAndUnmount asks QEMU to power down via QMP, waits for the PID
// file to disappear (force-killing on timeout), then unmounts every
// attached volume. Each step tolerates failure of the previous one.
func (m *Manager) shutdownAndUnmount(instance *VM) {
	if instance.QMPClient != nil {
		if _, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "system_powerdown"}, instance.ID); err != nil {
			slog.Warn("QMP system_powerdown failed (VM may already be stopped)",
				"id", instance.ID, "err", err)
		}
	}

	if err := utils.WaitForPidFileRemoval(instance.ID, pidFileRemovalTimeout); err != nil {
		slog.Warn("Timeout waiting for PID file removal", "id", instance.ID, "err", err)
		pid, readErr := utils.ReadPidFile(instance.ID)
		if readErr != nil {
			slog.Debug("No PID file found (VM likely already stopped)", "id", instance.ID)
		} else {
			slog.Info("Force killing process", "pid", pid, "id", instance.ID)
			if err := utils.KillProcess(pid); err != nil {
				slog.Error("Failed to kill process", "pid", pid, "id", instance.ID, "err", err)
			}
		}
	}

	if m.deps.VolumeMounter != nil {
		if err := m.deps.VolumeMounter.Unmount(instance); err != nil {
			slog.Error("Volume unmount failed", "id", instance.ID, "err", err)
		}
	}
}

// cleanupTapDevices removes the primary VPC tap, every extra ENI tap, and
// the management TAP/IP allocation. Errors are logged and tolerated.
func (m *Manager) cleanupTapDevices(instance *VM) {
	if instance.ENIId != "" && m.deps.NetworkPlumber != nil {
		if err := m.deps.NetworkPlumber.CleanupTap(TapDeviceName(instance.ENIId)); err != nil {
			slog.Warn("Failed to clean up tap device", "eni", instance.ENIId, "err", err)
		}
		m.cleanupExtraENITaps(instance)
	}

	if m.deps.InstanceCleaner != nil {
		m.deps.InstanceCleaner.CleanupMgmtNetwork(instance)
	}
}

// cleanupExtraENITaps removes tap devices for every extra ENI attached
// to a system VM (multi-subnet ALB instances span multiple ENIs).
func (m *Manager) cleanupExtraENITaps(instance *VM) {
	if m.deps.NetworkPlumber == nil {
		return
	}
	for _, extra := range instance.ExtraENIs {
		if err := m.deps.NetworkPlumber.CleanupTap(TapDeviceName(extra.ENIID)); err != nil {
			slog.Warn("Failed to clean up extra ENI tap device", "eni", extra.ENIID, "err", err)
		}
	}
}

// deallocateResources releases the per-instance vCPU/memory reservation
// back to the resource controller.
func (m *Manager) deallocateResources(instance *VM) {
	if m.deps.Resources == nil || instance.InstanceType == "" {
		return
	}
	m.deps.Resources.Deallocate(instance.InstanceType)
}

// transitionWithPrecheck validates the transition first, then calls the
// daemon-supplied TransitionState. Pre-validation lets us surface
// ErrInvalidTransition cleanly (so callers can map it to the AWS
// IncorrectInstanceState error code) and treat any post-precheck error
// as a persistence failure on a transition whose memory mutation
// already succeeded.
func (m *Manager) transitionWithPrecheck(instance *VM, target InstanceState) error {
	current := m.Status(instance)
	if !IsValidTransition(current, target) {
		return fmt.Errorf("%w: %s -> %s for instance %s",
			ErrInvalidTransition, current, target, instance.ID)
	}
	if m.deps.TransitionState == nil {
		// Inspect (not UpdateState): MarkFailed may run this on an instance
		// that was never inserted into the local map.
		m.Inspect(instance, func(v *VM) { v.Status = target })
		return nil
	}
	if err := m.deps.TransitionState(instance, target); err != nil {
		// Could be persistence failure (memory state already updated) or a
		// racing transition that invalidated the precheck. Re-inspect to
		// distinguish.
		if m.Status(instance) != target {
			return fmt.Errorf("%w: %s -> %s for instance %s (raced)",
				ErrInvalidTransition, current, target, instance.ID)
		}
		return err
	}
	return nil
}

// writeRunningState persists the current running-VM map via the StateStore.
// The View callback holds the manager lock across the marshal+put so VM
// fields can't change mid-encode; splitting marshal from put is deferred.
func (m *Manager) writeRunningState() error {
	if m.deps.StateStore == nil {
		return nil
	}
	var err error
	m.View(func(vms map[string]*VM) {
		err = m.deps.StateStore.SaveRunningState(m.deps.NodeID, vms)
	})
	return err
}
