package vm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// orphanQEMUKillTimeout bounds how long the reaper waits for a SIGKILL'd
// orphan QEMU to disappear before giving up and retrying on the next sweep.
const orphanQEMUKillTimeout = 10 * time.Second

// OrphanQEMUReaper reconciles instances whose QEMU liveness disagrees with their
// recorded state, in two directions:
//
//   - A terminated instance whose QEMU is still alive: TerminateInstances marked
//     the record terminated and drove node-local teardown, but if that teardown
//     ran on a node other than the one hosting the QEMU (e.g. the per-instance
//     subscription was lost and the request fell back to the ec2.terminate
//     broadcast), the process survives and keeps holding its OVN logical port /
//     IP, blocking rebuilds. The reaper force-kills it.
//   - A shutting-down instance whose QEMU has vanished: a terminate that
//     transitioned the instance to shutting-down but wedged downstream (a dead
//     nbdkit stalling the unmount seal, say) never reached finalizeTerminated,
//     stranding the instance and its volumes indefinitely. The reaper finalizes
//     the termination and hands outstanding teardown to TerminatedTeardownReaper.
//
// This reaper is node-local and conservative: liveness is read from the
// instance's local PID file, so only the node actually hosting (or having
// hosted) the process acts; every other node's lookup misses and is a no-op. A
// live in-progress terminate still has a live QEMU, so the shutting-down
// reconcile never steals a healthy teardown. Running, pending, and stopped
// instances are never touched.
type OrphanQEMUReaper struct {
	m *Manager
}

var _ Reaper = (*OrphanQEMUReaper)(nil)

// NewOrphanQEMUReaper builds the reaper bound to this Manager's state store.
func (m *Manager) NewOrphanQEMUReaper() *OrphanQEMUReaper {
	return &OrphanQEMUReaper{m: m}
}

func (r *OrphanQEMUReaper) Class() string      { return "orphan-qemu" }
func (r *OrphanQEMUReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep reaps any live QEMU whose instance is already terminated. A terminated
// instance must have no running process; one that does is an orphan holding
// OVN ports. The PID file is read by instance ID, so only the node hosting the
// process finds it alive — every other node's lookup misses and is a no-op.
func (r *OrphanQEMUReaper) Sweep(context.Context) (int, error) {
	if r.m.deps.StateStore == nil {
		return 0, nil
	}
	terminated, err := r.m.deps.StateStore.ListTerminatedInstances()
	if err != nil {
		return 0, fmt.Errorf("list terminated instances: %w", err)
	}

	reaped := 0
	for _, v := range terminated {
		pid, err := utils.ReadPidFile(v.ID)
		if err != nil {
			continue // no PID file on this node: process is not here
		}
		if !utils.ProcessAlive(pid) {
			_ = utils.RemovePidFile(v.ID) // stale PID file for a dead, terminated instance
			continue
		}

		slog.Warn("vm/gc: reaping orphan QEMU for terminated instance (held OVN ports)",
			"instanceId", v.ID, "pid", pid)
		if err := utils.ForceKillProcess(pid, orphanQEMUKillTimeout); err != nil {
			slog.Error("vm/gc: failed to reap orphan QEMU, will retry next sweep",
				"instanceId", v.ID, "pid", pid, "err", err)
			continue
		}
		_ = utils.RemovePidFile(v.ID)
		reaped++
	}

	// Second direction: finalize shutting-down instances whose QEMU vanished.
	// These live in this node's local running map (a wedged terminate never
	// migrated them to the terminated bucket), so scan the local snapshot. A
	// still-alive QEMU means the terminate is progressing normally — leave it.
	for _, v := range r.m.Snapshot() {
		if r.m.Status(v) != StateShuttingDown {
			continue
		}
		if qemuProcessAlive(v.ID) {
			continue
		}
		_ = utils.RemovePidFile(v.ID) // clear any stale file for the dead process

		slog.Warn("vm/gc: reconciling wedged shutting-down instance, QEMU vanished",
			"instanceId", v.ID)
		if err := r.m.reconcileVanishedQEMU(v); err != nil {
			slog.Error("vm/gc: failed to finalize wedged shutting-down instance, will retry next sweep",
				"instanceId", v.ID, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// qemuProcessAlive reports whether the instance's QEMU process is running on
// this node, read via its local PID file. A missing PID file (process not on
// this node, or already reaped) or a dead PID both read as not-alive. Used by
// the reaper, where a terminated/shutting-down instance's absent PID file
// legitimately means the process is gone.
func qemuProcessAlive(instanceID string) bool {
	pid, err := utils.ReadPidFile(instanceID)
	if err != nil {
		return false
	}
	return utils.ProcessAlive(pid)
}

// qemuConfirmedDead reports whether the instance's QEMU is provably gone: its
// PID file is present but the process is not alive. Unlike qemuProcessAlive, a
// missing PID file reads as NOT confirmed-dead (ambiguous), so a caller on a
// still-running instance (DetachVolume) falls through to its normal QMP path
// rather than short-circuiting on an absent file.
func qemuConfirmedDead(instanceID string) bool {
	pid, err := utils.ReadPidFile(instanceID)
	if err != nil {
		return false
	}
	return !utils.ProcessAlive(pid)
}
