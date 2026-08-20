package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DriftInterval is the gap between drift passes. Var (not const) so
// integration tests can shrink it.
var DriftInterval = 5 * time.Minute

// driftBackoffBase is the requeue gap after the first incomplete pass. Var so
// tests can shrink it.
var driftBackoffBase = 5 * time.Second

// driftBackoffFactor grows the requeue gap per consecutive incomplete pass, up
// to DriftInterval. Without a requeue the repair latency for any transient apply
// failure is a full DriftInterval, so a 57-second DHCP stall costs five minutes
// of a VPC having no external gateway.
const driftBackoffFactor = 3

// DriftLoop runs Reconcile every DriftInterval, gated on AcquireLeader so
// only one vpcd scans at a time. An incomplete pass requeues on a short
// backoff instead. Returns when ctx is cancelled.
func DriftLoop(ctx context.Context, rec Reconciler, nc *nats.Conn, localAZ, holder string) {
	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("reconcile/drift: JetStream context unavailable, drift loop disabled", "err", err)
		return
	}

	var backoff time.Duration
	timer := time.NewTimer(DriftInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			previous := backoff
			backoff = nextDriftBackoff(backoff, runDriftCycle(ctx, rec, nc, js, localAZ, holder))
			wait := driftWait(backoff)
			if backoff != previous {
				slog.Info("reconcile/drift: requeue interval changed", "wait_ms", otelsetup.Millis(wait))
			}
			timer.Reset(wait)
		}
	}
}

// nextDriftBackoff returns the requeue backoff after a pass: zero once a pass
// converges, otherwise the previous backoff tripled from driftBackoffBase and
// held at DriftInterval. Tracked separately from the wait it produces, so a
// backoff sitting at the cap is not mistaken for a converged loop and reset.
func nextDriftBackoff(current time.Duration, err error) time.Duration {
	if !errors.Is(err, ErrPassIncomplete) {
		return 0
	}
	next := driftBackoffBase
	if current > 0 {
		next = current * driftBackoffFactor
	}
	return min(next, DriftInterval)
}

// driftWait is the gap before the next pass for a backoff; no backoff means a
// full DriftInterval.
func driftWait(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		return DriftInterval
	}
	return backoff
}

// runDriftCycle is one tick body, split out so tests can drive it directly.
// Returns the reconcile outcome so the caller can size the next wait; a cycle
// this node did not win the election for returns nil.
func runDriftCycle(ctx context.Context, rec Reconciler, nc *nats.Conn, js jetstream.JetStream, localAZ, holder string) error {
	release, elected := AcquireLeader(ctx, nc, KVBucketVPCDReconcile, holder)
	if !elected {
		return nil
	}
	defer release()

	intent, err := LoadIntentFromKV(ctx, js, localAZ)
	if err != nil {
		slog.Warn("reconcile/drift: load intent failed", "err", err)
		return err
	}
	if err := rec.Reconcile(ctx, intent); err != nil {
		slog.Warn("reconcile/drift: reconcile failed", "err", err)
		return err
	}
	return nil
}
