package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// snapshotClaimReleaseTimeout bounds the start-up release, which lists every
// snapshot document before this daemon serves a request.
const snapshotClaimReleaseTimeout = 30 * time.Second

// releaseSnapshotClaims frees the claims this node's previous process held on
// pending snapshots, so the reaper is not held off them by a live heartbeat.
// A failed pass is retried in the background, where it skips this process's own.
func (d *Daemon) releaseSnapshotClaims() {
	if d.snapshotService == nil || d.snapshotClaims() == nil {
		return
	}
	if d.releaseSnapshotClaimsOnce(d.ctx) {
		return
	}
	d.shutdownWg.Go(func() {
		ticker := time.NewTicker(vm.GCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
				if d.releaseSnapshotClaimsOnce(d.ctx) {
					return
				}
			}
		}
	})
}

// releaseSnapshotClaimsOnce runs one release pass and reports whether it read
// and released everything it had to.
func (d *Daemon) releaseSnapshotClaimsOnce(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, snapshotClaimReleaseTimeout)
	defer cancel()
	released, err := d.snapshotService.ReleaseNodeClaims(ctx)
	if released > 0 {
		slog.InfoContext(ctx, "Released this node's claims on pending snapshots from its previous process", "count", released)
	}
	if err != nil {
		slog.WarnContext(ctx, "Failed to release this node's claims on pending snapshots, retrying in the background", "err", err)
		return false
	}
	return true
}
