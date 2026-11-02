package vpcd

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// Tunables for the flow-restore-wait watchdog. heldAfter is far longer than
// ovs-ctl's own restore, which clears the flag within seconds of a restart, so
// only a datapath that is never going to start forwarding is released.
var (
	vswitchdWatchdogInterval  = 15 * time.Second
	vswitchdWatchdogHeldAfter = 60 * time.Second
)

// flowRestoreProber is what the watchdog reads and writes on this host.
type flowRestoreProber interface {
	Held(ctx context.Context) (bool, error)
	Release(ctx context.Context) error
}

// hostFlowRestore probes the local ovs-vswitchd through the host package.
type hostFlowRestore struct{ r host.Runner }

func (h hostFlowRestore) Held(ctx context.Context) (bool, error) {
	return host.VswitchdHeldInFlowRestore(ctx, h.r)
}

func (h hostFlowRestore) Release(ctx context.Context) error {
	return host.ReleaseFlowRestoreWait(ctx, h.r)
}

// vswitchdWatchdog releases an ovs-vswitchd left in flow-restore-wait after a
// boot, where guests recovered onto the node are running but unreachable. Per
// host and not leader-gated, since only this host's datapath is held.
type vswitchdWatchdog struct {
	prober    flowRestoreProber
	heldAfter time.Duration

	// heldSince marks when the datapath was first seen held; zero otherwise.
	heldSince time.Time
}

func newVswitchdWatchdog(prober flowRestoreProber) *vswitchdWatchdog {
	return &vswitchdWatchdog{prober: prober, heldAfter: vswitchdWatchdogHeldAfter}
}

// evaluate runs one decision at time now and reports whether it released the
// datapath. A probe error counts as not held, so it never rewrites the flag.
func (w *vswitchdWatchdog) evaluate(ctx context.Context, now time.Time) bool {
	held, err := w.prober.Held(ctx)
	if err != nil {
		slog.Warn("ovs-vswitchd watchdog: flow-restore-wait probe failed", "err", err)
		return false
	}
	if !held {
		w.heldSince = time.Time{}
		return false
	}
	if w.heldSince.IsZero() {
		w.heldSince = now
	}
	if now.Sub(w.heldSince) < w.heldAfter {
		return false
	}

	slog.Warn("ovs-vswitchd watchdog: datapath held in flow-restore-wait with no upcall threads, releasing it",
		"held_for_ms", otelsetup.Millis(now.Sub(w.heldSince)))
	if err := w.prober.Release(ctx); err != nil {
		slog.Warn("ovs-vswitchd watchdog: clearing flow-restore-wait failed", "err", err)
		return false
	}
	w.heldSince = time.Time{}
	return true
}

// runVswitchdWatchdog ticks the watchdog until ctx is cancelled.
func runVswitchdWatchdog(ctx context.Context, w *vswitchdWatchdog) {
	ticker := time.NewTicker(vswitchdWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.evaluate(ctx, time.Now())
		}
	}
}
