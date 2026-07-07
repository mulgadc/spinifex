package vm

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// maxConcurrentRecovery bounds the recovery fan-out; cold-AMI clones are I/O-heavy.
const maxConcurrentRecovery = 2

// Restore loads persisted VM state and re-launches instances that are neither
// terminated nor user-stopped. Terminated/stopped instances migrate to shared KV;
// running instances with live QEMU reconnect via QMP; others relaunch via Run.
// All errors are logged; Restore never fails fatally.
func (m *Manager) Restore() {
	cleanShutdown := false
	if m.deps.ConsumeCleanShutdownMarker != nil {
		cleanShutdown = m.deps.ConsumeCleanShutdownMarker()
	}
	if !cleanShutdown {
		slog.Warn("No clean shutdown marker — possible crash recovery, validating QEMU PIDs carefully")
		time.Sleep(3 * time.Second)
	}

	if err := m.loadRunningState(); err != nil {
		slog.Warn("Failed to load state, continuing with empty state", "error", err)
		return
	}

	slog.Info("Loaded state", "instance count", m.Count())

	toLaunch := m.classifyRestoredInstances()

	if len(toLaunch) > 0 {
		m.relaunchAll(toLaunch)
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state after restore", "error", err)
	}
}

// loadRunningState replaces the manager's running set from the persisted snapshot.
// A missing key returns an empty map and no error.
func (m *Manager) loadRunningState() error {
	if m.deps.StateStore == nil {
		return fmt.Errorf("StateStore not wired")
	}
	loaded, err := m.deps.StateStore.LoadRunningState(m.deps.NodeID)
	if err != nil {
		return err
	}
	m.Replace(loaded)
	return nil
}

// classifyRestoredInstances routes each VM: stopped/terminated migrate to KV;
// running with live QEMU+NBD reconnects; transitional states finalize;
// the remainder is returned for relaunch via Manager.Run.
func (m *Manager) classifyRestoredInstances() []*VM {
	var toLaunch []*VM

	for _, instance := range m.Snapshot() {
		instance.EBSRequests.Mu = sync.Mutex{}
		instance.ENIRequests.Mu = sync.Mutex{}
		instance.QMPClient = &qmp.QMPClient{}

		if instance.Status == StateTerminated {
			if !m.MigrateTerminatedToKV(instance) {
				// KV write failed — keep in local state so the next restart
				// retries the migration. Deleting here would create a "void":
				// the instance disappears from both local state and the
				// terminated KV, making it invisible to DescribeInstances.
				slog.Warn("Terminated instance KV migration failed, will retry on next restart",
					"instance", instance.ID)
			}
			continue
		}

		if instance.Status == StateStopped {
			if !m.MigrateStoppedToSharedKV(instance) {
				// KV write failed — keep in local state so the next restart
				// retries the migration. Deleting here would create a "void":
				// the instance disappears from both local state and the
				// stopped KV, making it invisible to DescribeStoppedInstances.
				slog.Warn("Stopped instance KV migration failed, will retry on next restart",
					"instance", instance.ID)
			}
			continue
		}

		// Recovery-failed instances stay in StateError until the operator
		// explicitly retries or terminates. Skip relaunch and resource
		// re-allocation; resources were already released by stopCleanup
		// when MarkRecoveryFailed fired on the previous daemon run.
		if instance.Status == StateError {
			slog.Warn("Instance in error state; skipping recovery relaunch (operator must retry or terminate)",
				"instance", instance.ID, "managedBy", instance.ManagedBy, "instanceType", instance.InstanceType)
			continue
		}

		typeKnown := true
		if m.deps.InstanceTypes != nil {
			_, typeKnown = m.deps.InstanceTypes.Resolve(instance.InstanceType)
		}
		if !typeKnown && instance.InstanceType != "" {
			slog.Warn("Instance type not available on this node, moving to stopped",
				"instanceId", instance.ID, "instanceType", instance.InstanceType)
			markUnschedulable(instance,
				fmt.Sprintf("instance type %s is not available on this node", instance.InstanceType))
			m.MigrateStoppedToSharedKV(instance)
			continue
		}

		if typeKnown && m.deps.Resources != nil && instance.InstanceType != "" {
			slog.Info("Re-allocating resources for instance", "instanceId", instance.ID, "type", instance.InstanceType)
			if err := m.deps.Resources.Allocate(instance.InstanceType); err != nil {
				slog.Error("Failed to re-allocate resources for instance on startup, moving to stopped",
					"instanceId", instance.ID, "err", err)
				markUnschedulable(instance,
					fmt.Sprintf("insufficient resources to restore instance: %v", err))
				m.MigrateStoppedToSharedKV(instance)
				continue
			}
		}

		if isInstanceProcessRunning(instance) {
			if !AreVolumeSocketsValid(instance) {
				slog.Warn("QEMU alive but NBD sockets are stale, killing orphaned process for relaunch",
					"instance", instance.ID)
				if !killOrphanedQEMU(instance) {
					continue
				}
			} else {
				slog.Info("Instance QEMU process still alive, reconnecting", "instance", instance.ID)
				if err := m.reconnectInstance(instance); err != nil {
					slog.Error("Failed to reconnect to running instance, marking recovery-failed to preserve user data",
						"instanceId", instance.ID, "err", err)
					m.MarkRecoveryFailed(instance, "reconnect_failed")
				}
				continue
			}
		}

		// QEMU is not running -- resolve transitional states from interrupted operations.
		switch instance.Status {
		case StateStopping, StateShuttingDown:
			if m.finalizeTransitionalRestore(instance) {
				continue
			}
			continue
		case StateRunning:
			instance.Status = StatePending
			slog.Info("Instance was running but QEMU exited, relaunching", "instance", instance.ID)
		}

		// Reset LaunchTime so the pending watchdog gives a fresh timeout window.
		// Without this, the stale LaunchTime from the original launch causes the
		// watchdog to immediately mark the instance as failed after a prolonged outage.
		now := time.Now()
		if instance.Instance != nil {
			instance.Instance.LaunchTime = &now
		}
		toLaunch = append(toLaunch, instance)
	}

	return toLaunch
}

// markUnschedulable flips an instance to Stopped with InsufficientInstanceCapacity
// so DescribeInstances reports a useful error when the node can no longer host the type.
func markUnschedulable(instance *VM, reason string) {
	instance.Status = StateStopped
	if instance.Instance != nil {
		instance.Instance.StateReason = &ec2.StateReason{}
		instance.Instance.StateReason.SetCode("Server.InsufficientInstanceCapacity")
		instance.Instance.StateReason.SetMessage(reason)
	}
}

// killOrphanedQEMU SIGKILLs a QEMU whose NBD storage is no longer reachable.
// Returns true when the process is gone and classification can proceed.
// SIGKILL is used directly to avoid the 120s SIGTERM timeout blocking startup.
func killOrphanedQEMU(instance *VM) bool {
	pid, pidErr := utils.ReadPidFile(instance.ID)
	if pidErr != nil || pid <= 0 {
		slog.Error("Cannot read PID for orphaned QEMU, skipping relaunch",
			"instanceId", instance.ID, "err", pidErr)
		return false
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
	// SIGKILL cannot be caught; QEMU never runs its cleanup so the PID
	// file stays on disk. Wait for the process to die, then remove it.
	if err := utils.WaitForProcessExit(pid, 10*time.Second); err != nil {
		slog.Error("Orphaned QEMU did not exit after SIGKILL, skipping relaunch",
			"instanceId", instance.ID, "pid", pid, "err", err)
		return false
	}
	_ = utils.RemovePidFile(instance.ID)
	return true
}

// finalizeTransitionalRestore advances a Stopping/ShuttingDown instance (whose
// QEMU is gone) to its stable state and migrates it to the appropriate KV bucket.
// Returns true on success; false signals the caller to retry on next restart.
func (m *Manager) finalizeTransitionalRestore(instance *VM) bool {
	prevStatus := instance.Status
	if instance.Status == StateStopping {
		instance.Status = StateStopped
	} else {
		instance.Status = StateTerminated
	}
	slog.Info("QEMU exited during transition, finalizing state",
		"instance", instance.ID, "from", prevStatus, "to", instance.Status)

	if instance.Status == StateStopped && m.MigrateStoppedToSharedKV(instance) {
		return true
	}
	if instance.Status == StateTerminated && m.MigrateTerminatedToKV(instance) {
		return true
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state, will retry on next restart",
			"instance", instance.ID, "error", err)
		instance.Status = prevStatus // revert so next restart retries
	}
	return true
}

// relaunchAll fires OnInstanceRecovering for each instance (for early
// ec2.cmd.<id> subscription) then fans out Manager.Run under a semaphore.
func (m *Manager) relaunchAll(toLaunch []*VM) {
	if m.deps.Hooks.OnInstanceRecovering != nil {
		for _, instance := range toLaunch {
			m.deps.Hooks.OnInstanceRecovering(instance)
		}
	}

	slog.Info("Launching instances (recovery)", "count", len(toLaunch), "maxConcurrent", maxConcurrentRecovery)
	sem := make(chan struct{}, maxConcurrentRecovery)
	var wg sync.WaitGroup

	for _, instance := range toLaunch {
		sem <- struct{}{}
		wg.Add(1)
		go func(inst *VM) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Panic during instance recovery",
						"instanceId", inst.ID, "panic", r, "stack", string(debug.Stack()))
				}
			}()

			status := m.Status(inst)
			if status != StatePending && status != StateProvisioning {
				slog.Info("Instance state changed during recovery, skipping launch",
					"instanceId", inst.ID, "status", string(status))
				return
			}
			if m.deps.Hooks.BeforeInstanceRelaunch != nil {
				if err := m.deps.Hooks.BeforeInstanceRelaunch(inst); err != nil {
					slog.Error("Pre-relaunch hook failed",
						"instanceId", inst.ID, "managedBy", inst.ManagedBy,
						"instanceType", inst.InstanceType, "err", err)
					m.MarkRecoveryFailed(inst, "pre_relaunch_hook_failed")
					return
				}
			}
			slog.Info("Launching instance (recovery)",
				"instance", inst.ID, "managedBy", inst.ManagedBy, "instanceType", inst.InstanceType)
			// Restore runs at daemon start with no request context.
			if err := m.Run(context.Background(), inst); err != nil {
				slog.Error("Failed to launch instance during recovery",
					"instanceId", inst.ID, "managedBy", inst.ManagedBy, "instanceType", inst.InstanceType, "err", err)
				m.MarkRecoveryFailed(inst, "recovery_launch_failed")
			}
		}(instance)
	}
	wg.Wait()
}

// attachQMPForReconnect is a test seam over (*Manager).AttachQMP so tests can
// drive reconnectInstance without spawning the 30s heartbeat goroutine (goleak).
var attachQMPForReconnect = (*Manager).AttachQMP

// reconnectInstance re-establishes QMP for a surviving QEMU, fires OnInstanceUp
// to reinstall NATS subscriptions, and persists running state. Subscribe failure
// closes QMP and propagates the error; status is only set to Running after
// subscriptions are confirmed live to avoid advertising a broken instance.
func (m *Manager) reconnectInstance(instance *VM) error {
	if err := attachQMPForReconnect(m, instance); err != nil {
		return fmt.Errorf("failed to reconnect QMP: %w", err)
	}

	if m.deps.Hooks.OnInstanceUp != nil {
		if err := m.deps.Hooks.OnInstanceUp(instance); err != nil {
			if instance.QMPClient != nil && instance.QMPClient.Conn != nil {
				_ = instance.QMPClient.Conn.Close()
				instance.QMPClient = nil
			}
			return fmt.Errorf("failed to reinstall per-instance NATS subscriptions: %w", err)
		}
	}

	instance.Status = StateRunning

	// Re-assert boot-volume in-use state; a daemon restart otherwise leaves the
	// root volume "available" while the instance runs (split-brain, siv-464).
	m.markBootVolumesInUse(instance)

	if err := m.writeRunningState(); err != nil {
		return fmt.Errorf("failed to persist reconnected instance state: %w", err)
	}

	slog.Info("Successfully reconnected to running instance", "instance", instance.ID)
	return nil
}

// isInstanceProcessRunning reports whether the QEMU process in the PID file
// is still alive. Returns false on any failure (missing file, dead PID).
func isInstanceProcessRunning(instance *VM) bool {
	pid, err := utils.ReadPidFile(instance.ID)
	if err != nil || pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// AreVolumeSocketsValid dials each NBD Unix socket to confirm viperblock is
// still listening. A stat-only check is insufficient since viperblock may restart
// leaving stale socket files. TCP and unparseable URIs are treated as valid.
func AreVolumeSocketsValid(instance *VM) bool {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	for _, req := range instance.EBSRequests.Requests {
		if req.NBDURI == "" {
			continue
		}
		serverType, sockPath, _, _, err := utils.ParseNBDURI(req.NBDURI)
		if err != nil || serverType != "unix" {
			continue
		}
		conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
		if err != nil {
			slog.Debug("NBD socket unreachable", "volume", req.Name, "socket", sockPath, "err", err)
			return false
		}
		_ = conn.Close()
	}
	return true
}
