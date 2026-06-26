package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// DriftInterval is the gap between drift passes. Var (not const) so
// integration tests can shrink it.
var DriftInterval = 5 * time.Minute

// DriftLoop runs Reconcile every DriftInterval, gated on AcquireLeader so
// only one vpcd scans at a time. Returns when ctx is cancelled.
func DriftLoop(ctx context.Context, rec Reconciler, nc *nats.Conn, localAZ, holder string) {
	js, err := nc.JetStream()
	if err != nil {
		slog.Error("reconcile/drift: JetStream context unavailable, drift loop disabled", "err", err)
		return
	}

	ticker := time.NewTicker(DriftInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runDriftCycle(ctx, rec, nc, js, localAZ, holder)
		}
	}
}

// runDriftCycle is one tick body, split out so tests can drive it directly.
func runDriftCycle(ctx context.Context, rec Reconciler, nc *nats.Conn, js nats.JetStreamContext, localAZ, holder string) {
	release, elected := AcquireLeader(nc, KVBucketVPCDReconcile, holder)
	if !elected {
		return
	}
	defer release()

	intent, err := LoadIntentFromKV(ctx, js, localAZ)
	if err != nil {
		slog.Warn("reconcile/drift: load intent failed", "err", err)
		return
	}
	if err := rec.Reconcile(ctx, intent); err != nil {
		slog.Warn("reconcile/drift: reconcile failed", "err", err)
	}
}
