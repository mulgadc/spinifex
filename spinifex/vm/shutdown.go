package vm

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// pidFileRemovalTimeout is how long Stop/Terminate wait for the PID file to
// disappear after system_powerdown before resorting to SIGKILL.
const pidFileRemovalTimeout = 20 * time.Second

// Stop transitions a running instance to stopped: graceful QMP shutdown, volume
// unmount, tap teardown, resource deallocation. Migrates to the "stopped" KV
// bucket and fires OnInstanceDown. Returns ErrInstanceNotFound or ErrInvalidTransition.
func (m *Manager) Stop(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}

	migrated, err := m.stopOne(instance)
	if err != nil {
		return err
	}
	if !migrated {
		return nil
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

// stopOne runs the stop sequence shared by Stop and StopAll:
// Stopping → stopCleanup → Stopped → migrate to "stopped" KV → OnInstanceDown.
// Returns (true, nil) when migration removed the instance (caller must persist).
// Returns (false, nil) on KV failure or slot reclaim; (false, err) on precheck failure.
func (m *Manager) stopOne(instance *VM) (bool, error) {
	if err := m.transitionWithPrecheck(instance, StateStopping); err != nil {
		return false, err
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
		return false, nil
	}

	if m.deps.Hooks.OnInstanceDown != nil {
		m.deps.Hooks.OnInstanceDown(instance.ID)
	}
	return true, nil
}

// StopAll fans stopOne across every VM for the coordinated shutdown DRAIN phase.
// Runs one goroutine per VM; per-VM errors are logged but do not abort the fan-out.
// AWS resources (ENI, public IP, placement group) are not released on stop.
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
			if _, err := m.stopOne(v); err != nil {
				if errors.Is(err, ErrInvalidTransition) {
					slog.Debug("StopAll: skipping non-running instance",
						"instanceId", v.ID, "state", string(m.Status(v)))
					return
				}
				slog.Error("StopAll: stopOne failed", "instanceId", v.ID, "err", err)
			}
		}(instance)
	}
	wg.Wait()
	if err := m.writeRunningState(); err != nil {
		slog.Error("StopAll: failed to persist running state after fan-out", "err", err)
		return err
	}
	return nil
}

// Terminate transitions an instance to terminated: graceful shutdown, volume +
// ENI + IP cleanup, placement group removal. Idempotent on already-shutting-down.
// Returns ErrInstanceNotFound or ErrInvalidTransition as appropriate.
func (m *Manager) Terminate(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		// Idempotent terminate (rule #1): an absent instance is already gone,
		// so destroy retries converge.
		return nil
	}

	if current := m.Status(instance); current == StateShuttingDown || current == StateTerminated {
		// Already terminating/terminated: cleanup is owned elsewhere. Idempotent.
		return nil
	}

	if err := m.transitionWithPrecheck(instance, StateShuttingDown); err != nil {
		return err
	}

	m.terminateCleanup(instance)

	return m.finalizeTerminated(instance)
}

// MarkFailed sets a failure reason, transitions to shutting-down synchronously,
// then runs the cleanup chain in a goroutine so callers return immediately.
// Tolerates instances already in a cleanup state (no-op).
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

	m.goroutineWg.Go(func() {
		m.terminateCleanup(instance)
		if err := m.finalizeTerminated(instance); err != nil {
			slog.Error("MarkFailed finalize failed", "instanceId", instance.ID, "err", err)
		}
	})
}

// MarkRecoveryFailed transitions an instance to StateError after a failed
// daemon-restart recovery. Runs non-destructive cleanup (unmount, tap teardown,
// GPU/resource release) in a goroutine. Unlike MarkFailed, volumes, ENIs, and
// IPs are preserved for operator retry or explicit TerminateInstances.
func (m *Manager) MarkRecoveryFailed(instance *VM, reason string) {
	skip := false
	var observed InstanceState
	m.Inspect(instance, func(v *VM) {
		observed = v.Status
		if v.Status == StateError || v.Status == StateShuttingDown || v.Status == StateTerminated {
			skip = true
			return
		}
		if v.Instance != nil {
			v.Instance.StateReason = &ec2.StateReason{
				Code:    aws.String("Server.RecoveryFailed"),
				Message: aws.String(reason),
			}
		}
	})
	if skip {
		slog.Info("MarkRecoveryFailed: instance already in terminal/cleanup state, skipping",
			"instanceId", instance.ID, "status", string(observed), "reason", reason)
		return
	}

	if err := m.transitionWithPrecheck(instance, StateError); err != nil {
		slog.Error("MarkRecoveryFailed transition failed", "instanceId", instance.ID, "err", err)
		if m.Status(instance) != StateError {
			return
		}
	}
	slog.Error("Instance marked recovery_failed; volumes and ENIs preserved for operator action",
		"instanceId", instance.ID, "reason", reason)

	m.goroutineWg.Go(func() {
		m.stopCleanup(instance)
		m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })
		if err := m.writeRunningState(); err != nil {
			slog.Error("Failed to persist state after recovery failure",
				"instanceId", instance.ID, "err", err)
		}
	})
}

// finalizeTerminated transitions to terminated, writes the KV entry, removes
// from the local map, fires OnInstanceDown, and persists the running set.
func (m *Manager) finalizeTerminated(instance *VM) error {
	// Inspect (not UpdateState): MarkFailed may invoke this for an
	// instance that was never inserted into the local map.
	m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })

	if err := m.transitionWithPrecheck(instance, StateTerminated); err != nil {
		return fmt.Errorf("transition to terminated: %w", err)
	}

	// Stamp the termination time so the GC backstop can preserve a
	// describe-visibility window before reclaiming the record early.
	if instance.TerminatedAt.IsZero() {
		instance.TerminatedAt = time.Now()
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
		if err := m.deps.InstanceCleaner.ReleaseGPU(instance); err != nil {
			slog.Warn("ReleaseGPU failed on stop", "instanceId", instance.ID, "err", err)
		}
	}
	m.deallocateResources(instance)
}

// terminateCleanup is stopCleanup plus the AWS-resource cleanup that
// only applies on terminate: volume deletion, public IP release, ENI
// deletion, placement-group removal.
func (m *Manager) terminateCleanup(instance *VM) {
	m.shutdownAndUnmount(instance)
	m.markTeardown(instance, TeardownQEMU, TeardownDone)

	if m.deps.InstanceCleaner != nil {
		m.markTeardownResult(instance, TeardownVolumes, m.deps.InstanceCleaner.DeleteVolumes(instance))
	}

	m.cleanupTapDevices(instance)
	m.markTeardown(instance, TeardownTap, TeardownDone)

	if m.deps.InstanceCleaner != nil {
		gpuErr := m.deps.InstanceCleaner.ReleaseGPU(instance)
		if len(instance.GPUAttachments) > 0 {
			m.markTeardownResult(instance, TeardownGPU, gpuErr)
		}

		// Public IP: ReleaseIP is sync; vpc.delete-nat is fire-and-forget, so the
		// NAT rule removal is recorded pending (drift reconciler / GC reaps it).
		natErr := m.deps.InstanceCleaner.ReleasePublicIP(instance)
		if instance.PublicIP != "" {
			if natErr != nil {
				m.markTeardown(instance, TeardownNAT, TeardownFailed)
			} else {
				m.markTeardown(instance, TeardownNAT, TeardownPending)
			}
		}

		// ENI KV delete is sync; vpc.delete-port (OVN LSP) is fire-and-forget, so
		// the OVN port removal is recorded pending (reconcile LSP prune reaps it).
		eniErr := m.deps.InstanceCleaner.DetachAndDeleteENI(instance)
		if instance.ENIId != "" {
			m.markTeardownResult(instance, TeardownENI, eniErr)
			m.markTeardown(instance, TeardownOVN, TeardownPending)
		}

		placementErr := m.deps.InstanceCleaner.RemoveFromPlacementGroup(instance)
		if instance.PlacementGroupName != "" {
			m.markTeardownResult(instance, TeardownPlacement, placementErr)
		}
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

	for _, fw := range instance.Config.FwCfg {
		if err := os.Remove(fw.File); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove fw_cfg temp file", "file", fw.File, "id", instance.ID, "err", err)
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
// back to the resource controller. The single release/restore chokepoint for
// stop, terminate and crash recovery.
func (m *Manager) deallocateResources(instance *VM) {
	if m.deps.Resources == nil || instance.InstanceType == "" {
		return
	}
	// A reservation-bound instance returns its slot to the reservation, not the
	// general pool. CapacityReservationId is set at launch and only cleared (under
	// the manager lock, on crash before a general-capacity restart), so the
	// stop/terminate/crash reads here do not overlap that clear.
	if instance.CapacityReservationId != "" {
		m.deps.Resources.ReleaseToReservation(instance.CapacityReservationId, instance.InstanceType)
		return
	}
	m.deps.Resources.Deallocate(instance.InstanceType)
}

// transitionWithPrecheck validates the transition then calls TransitionState.
// Surfaces ErrInvalidTransition cleanly; post-precheck errors are persistence
// failures on a transition whose in-memory mutation already succeeded.
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

// writeRunningState persists the running-VM map. View holds the lock across
// marshal+put to prevent field changes mid-encode.
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
