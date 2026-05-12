package daemon

import (
	"log/slog"
)

// reconcileOnHeal runs the post-heal resync. Fires from two edges:
//   - onNATSReconnect, when the local NATS client reattaches.
//   - probePeersOnce, when peer reachability flips false→true (the only
//     signal in Scenario C where the NATS client stayed connected to its
//     local server throughout the partition).
//
// Both edges may fire near-simultaneously on the same heal. reconciling
// coalesces them: the second caller observes the flag set and returns,
// keeping the work to one round-trip.
//
// The caller is expected to invoke this in a goroutine — both call sites
// run on latency-sensitive callbacks (NATS client thread, peer-probe
// ticker) and the reconcile path performs KV I/O.
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

	// Push fresh local running-instance state. This re-establishes our
	// row in the cluster-wide instance-state bucket regardless of what
	// it held mid-partition (stale, missing, or clobbered by a peer's
	// stream snapshot restore).
	if err := d.WriteState(); err != nil {
		slog.Warn("reconcileOnHeal: WriteState failed", "reason", reason, "error", err)
		return
	}

	// NOTE: an earlier revision called d.ensureDefaultVPCInfrastructure()
	// here. DDIL Scenario C regressed because the very first peer-probe
	// tick on multi-node startup flips peersReachable false→true and
	// triggers this path concurrently with startCluster's own
	// ensureDefaultVPCInfrastructure call. The Describe→Create→Attach
	// sequence is not cluster-wide singleton, so multiple daemons +
	// double calls per daemon could race and produce duplicate IGW
	// attachments, corrupting default-VPC routing. Heal-time bootstrap
	// re-fire needs leader election or a debounce against the
	// startup-time path before re-introducing.

	slog.Info("reconcileOnHeal: complete", "reason", reason, "revision", d.Revision())
}
