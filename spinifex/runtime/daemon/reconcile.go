package daemon

import (
	"log/slog"
)

// reconcileOnHeal runs the post-heal resync (WriteState to KV). Coalesces
// concurrent calls via reconciling flag. Must be called in a goroutine.
func (d *Daemon) reconcileOnHeal(reason string) {
	if d.jsManager == nil {
		return
	}
	if !d.reconciling.CompareAndSwap(false, true) {
		slog.Debug("reconcileOnHeal: already running, coalescing", "reason", reason)
		return
	}
	defer d.reconciling.Store(false)

	slog.Info("reconcileOnHeal: starting", "reason", reason, "node", d.node)

	if err := d.WriteState(); err != nil {
		slog.Warn("reconcileOnHeal: WriteState failed", "reason", reason, "error", err)
		return
	}

	// ensureDefaultVPCInfrastructure is intentionally omitted: the peer-probe
	// heal edge and startCluster race without singleton protection and can
	// produce duplicate IGW attachments. Re-introduce only with leader election.

	slog.Info("reconcileOnHeal: complete", "reason", reason, "revision", d.Revision())
}
