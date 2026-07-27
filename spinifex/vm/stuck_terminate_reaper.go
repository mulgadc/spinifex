package vm

import (
	"context"
	"log/slog"
	"time"
)

// stuckTerminateTimeout is how long an instance may sit in shutting-down before
// the backstop force-completes its terminate. It is deliberately far longer than
// a healthy terminate (graceful powerdown plus the unmount seal) and longer than
// OrphanQEMUReaper's dead-QEMU reconcile window, so the gentler paths always get
// first chance; only a genuinely wedged terminate ever reaches this.
const stuckTerminateTimeout = 10 * time.Minute

// StuckTerminateReaper is the delete-authorized terminate backstop. When a
// terminate wedges with QEMU still alive — so OrphanQEMUReaper's dead-QEMU
// reconcile never fires — and stays shutting-down past stuckTerminateTimeout,
// this reaper force-kills the wedged process, reclaims DeleteOnTermination
// volume space, and drives the record to terminated.
//
// It is the one reaper explicitly authorized to DELETE volume data, but only for
// a DeleteOnTermination volume whose instance is already being terminated — a far
// narrower licence than the one ADR-0005 §3 denies VolumeLeakReaper. VolumeLeakReaper
// stays mark-and-alarm-only for every merely-orphaned volume; this actor only ever
// finishes an already-decided terminate, never reclaims a volume on its own judgement.
type StuckTerminateReaper struct {
	m *Manager
}

var _ Reaper = (*StuckTerminateReaper)(nil)

// NewStuckTerminateReaper builds the backstop bound to this Manager.
func (m *Manager) NewStuckTerminateReaper() *StuckTerminateReaper {
	return &StuckTerminateReaper{m: m}
}

func (r *StuckTerminateReaper) Class() string      { return "stuck-terminate" }
func (r *StuckTerminateReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep force-completes terminates that have been wedged in shutting-down past
// stuckTerminateTimeout. Wedged instances live in this node's local running map
// (a stuck terminate never migrated them to the terminated bucket), so scan the
// local snapshot.
func (r *StuckTerminateReaper) Sweep(context.Context) (int, error) {
	reaped := 0
	for _, v := range r.m.Snapshot() {
		if r.m.Status(v) != StateShuttingDown {
			continue
		}
		// A terminate still shutting-down only briefly is progressing normally,
		// and a dead-QEMU wedge is OrphanQEMUReaper's job. Only one stuck long
		// past the timeout is force-completed here. A missing timestamp (never
		// stamped, or lost across a restart) reads as not-yet-stuck, so the
		// backstop stays conservative and never acts on an unbounded record.
		if v.ShuttingDownAt.IsZero() || time.Since(v.ShuttingDownAt) < stuckTerminateTimeout {
			continue
		}

		slog.Warn("vm/gc: force-completing terminate wedged past timeout",
			"instanceId", v.ID, "shuttingDownFor", time.Since(v.ShuttingDownAt))
		if err := r.m.forceFinalizeStuckTerminate(v); err != nil {
			slog.Error("vm/gc: failed to force-complete wedged terminate, will retry next sweep",
				"instanceId", v.ID, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}
