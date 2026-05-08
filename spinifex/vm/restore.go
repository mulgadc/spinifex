package vm

import (
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

// maxConcurrentRecovery limits how many VMs are relaunched in parallel during
// recovery. Cold-AMI clones plus QEMU exec start are I/O-heavy; bounding the
// fan-out keeps the host responsive while still parallelising the slow path.
const maxConcurrentRecovery = 2

// Restore loads persisted VM state and re-launches instances that are
// neither terminated nor flagged as user-stopped. Steps:
//
//  1. Consume the clean-shutdown marker (deps callback). Without one, sleep
//     briefly so any stale QEMU PIDs from a crashed previous run finish dying
//     before isInstanceProcessRunning is consulted.
//  2. Load the per-node running snapshot via StateStore.
//  3. Classify each instance — terminated/stopped go to their shared KV
//     buckets; running with live QEMU + valid sockets reconnects via QMP;
//     transitional states finalise; everything else queues for relaunch.
//  4. Fan out Manager.Run across the relaunch queue with a semaphore.
//
// All errors are logged; Restore never fails fatally — it always leaves the
// daemon in a usable state.
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

// loadRunningState replaces the manager's running set with the snapshot
// persisted for this node. Returns an error only if the StateStore
// surfaced one — a missing key produces an empty map and no error.
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

// classifyRestoredInstances walks every loaded VM and routes it: stopped
// and terminated instances go to their shared KV buckets and are dropped
// from the local map; running instances with live QEMU + reachable NBD
// sockets reconnect via QMP; transitional states (Stopping, ShuttingDown)
// finalise to their stable counterparts; the remainder is returned for
// relaunch via Manager.Run.
func (m *Manager) classifyRestoredInstances() []*VM {
	var toLaunch []*VM

	for _, instance := range m.Snapshot() {
		instance.EBSRequests.Mu = sync.Mutex{}
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
					slog.Error("Failed to reconnect to running instance, marking failed to tear down orphaned QEMU",
						"instanceId", instance.ID, "err", err)
					m.MarkFailed(instance, "reconnect_failed")
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

// markUnschedulable flips an instance to Stopped with an
// InsufficientInstanceCapacity StateReason so DescribeInstances surfaces a
// useful error after a node loses the ability to host the type.
func markUnschedulable(instance *VM, reason string) {
	instance.Status = StateStopped
	if instance.Instance != nil {
		instance.Instance.StateReason = &ec2.StateReason{}
		instance.Instance.StateReason.SetCode("Server.InsufficientInstanceCapacity")
		instance.Instance.StateReason.SetMessage(reason)
	}
}

// killOrphanedQEMU SIGKILLs a QEMU process whose NBD storage no longer
// works (viperblock restarted with fresh sockets). Returns true when the
// process is gone (or was never running) and the caller should proceed
// with classification; false when the kill failed and the instance
// should be skipped this cycle.
//
// SIGKILL directly: orphaned QEMU with dead storage has no state worth a
// graceful shutdown, and KillProcess's 120s SIGTERM timeout would block
// daemon startup.
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

// finalizeTransitionalRestore resolves an instance whose QEMU is gone but
// whose persisted state was Stopping or ShuttingDown — flipping it to its
// stable counterpart and migrating to the appropriate shared KV bucket.
// Returns true on a clean migration; false signals the caller to retry on
// the next restart.
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

// relaunchAll kicks the recovery launch loop. Each instance is announced
// via OnInstanceRecovering before launching so the daemon can
// early-subscribe ec2.cmd.<id>; that lets a concurrent terminate reach
// this node while the relaunch is still pending. The OnInstanceUp hook
// fired after a successful launch reinstalls both per-instance subs
// idempotently.
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
			slog.Info("Launching instance (recovery)", "instance", inst.ID)
			if err := m.Run(inst); err != nil {
				slog.Error("Failed to launch instance during recovery", "instanceId", inst.ID, "err", err)
				m.MarkFailed(inst, "recovery_launch_failed")
			}
		}(instance)
	}
	wg.Wait()
}

// reconnectInstance re-establishes the QMP connection for an instance whose
// QEMU survived the daemon restart, fires OnInstanceUp so the daemon
// reinstalls per-instance NATS subscriptions, and persists the running
// state. Bypasses the state-machine validation because reconnect is not a
// modelled transition.
//
// Subscribe failure during the hook is treated as a hard error: the QMP
// connection is closed (so the heartbeat goroutine exits on the next tick
// when status leaves StateRunning) and the error is propagated. The caller
// in classifyRestoredInstances logs and moves on; the instance remains in
// the loaded map and will be retried on the next daemon restart. Status is
// only flipped to StateRunning after subscribes are confirmed live so a
// failed reconnect does not advertise a half-working instance to peers.
func (m *Manager) reconnectInstance(instance *VM) error {
	if err := m.AttachQMP(instance); err != nil {
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

	if err := m.writeRunningState(); err != nil {
		return fmt.Errorf("failed to persist reconnected instance state: %w", err)
	}

	slog.Info("Successfully reconnected to running instance", "instance", instance.ID)
	return nil
}

// isInstanceProcessRunning checks whether the QEMU process recorded in
// the instance's PID file is still alive. Returns false on any failure
// path (missing PID file, dead PID, signal-0 error).
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

// AreVolumeSocketsValid reports whether the NBD Unix sockets backing the
// instance's volumes are reachable. A dial probe (not just os.Stat) is
// required because viperblock may restart with sockets at the same paths
// — the file exists but no process is listening on the old fd that QEMU
// holds. TCP and unparseable URIs are treated as valid (we can't probe
// remote viperblockd from the recovery path).
//
// Exported so the daemon-side test suite can assert socket-validity
// behaviour through the existing test helper without reaching into
// manager internals.
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
