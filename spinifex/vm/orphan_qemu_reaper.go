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

// OrphanQEMUReaper kills QEMU processes left running for instances that are
// already terminated. TerminateInstances marks the EC2 record terminated and
// drives node-local teardown, but if that teardown ran on a node other than the
// one hosting the QEMU (e.g. the per-instance subscription was lost and the
// request fell back to the ec2.terminate broadcast), the process survives and
// keeps holding its OVN logical port / IP, blocking rebuilds.
//
// This reaper is node-local and conservative: it only inspects instances in the
// terminated KV bucket and only acts when a live process is found via the
// instance's local PID file. Running, pending, and stopped instances are never
// touched, so a launching or healthy VM can never be reaped. Whichever node
// actually holds the orphan PID file is the one that reaps it.
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
	return reaped, nil
}
