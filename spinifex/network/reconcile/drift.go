package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// DriftInterval is the gap between drift-detection passes. Five minutes is
// loose enough that two minutes of NATS KV silence after a successful boot
// reconcile is observable as "no drift" before the next tick; tight enough
// that a missed NATS event surfaces within the same support call.
//
// Var (not const) so integration tests can shrink it.
var DriftInterval = 5 * time.Minute

// DriftLoop runs Reconcile every DriftInterval, gated on AcquireLeader so
// only one vpcd in the cluster scans at a time. Returns when ctx is
// cancelled.
//
// On each tick: acquire leader → load intent → reconcile → release. A
// missed acquire (another vpcd is leader, or JetStream is unreachable) is
// logged at Debug and the tick is skipped — the next tick retries.
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

// runDriftCycle is one tick body — split out so tests can drive it
// directly without spinning the goroutine.
func runDriftCycle(ctx context.Context, rec Reconciler, nc *nats.Conn, js nats.JetStreamContext, localAZ, holder string) {
	release, elected := AcquireLeader(nc, holder)
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
