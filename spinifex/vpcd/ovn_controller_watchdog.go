package vpcd

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/host"
)

// Compile-time check: the gateway-claim prober satisfies the watchdog's needs.
var _ sbStateProber = (*host.GatewayClaimProber)(nil)

// Tunables for the ovn-controller wedge watchdog. Package vars so tests can shrink
// them. staleAfter must exceed a normal reconnect blip so a transient
// non-"connected" status does not trip a reset; cooldown bounds the reset rate so a
// genuinely-down SB does not induce a tight reset loop.
var (
	ovnWatchdogInterval   = 15 * time.Second
	ovnWatchdogStaleAfter = 45 * time.Second
	ovnWatchdogCooldown   = 60 * time.Second
)

// sbStateProber is the slice of the gateway-claim prober the watchdog needs.
// *host.GatewayClaimProber satisfies it; tests supply a fake.
type sbStateProber interface {
	SBConnectionState(ctx context.Context) (string, error)
	ResetSBClusterState(ctx context.Context) error
}

// ovnWatchdog detects a wedged local ovn-controller — one whose SB OVSDB client is
// stuck on stale RAFT cluster state and has stopped realising Port_Bindings — and
// clears it with sb-cluster-state-reset. It is per host, not leader-gated: the wedge
// is local to a chassis and usually strikes a non-leader placement host, so a
// leader-only recovery would never fire on the wedged node.
type ovnWatchdog struct {
	prober     sbStateProber
	staleAfter time.Duration
	cooldown   time.Duration

	// nonConnectedSince marks when the status first went non-"connected"; zero while
	// connected. lastReset gates the cooldown; zero until the first reset.
	nonConnectedSince time.Time
	lastReset         time.Time
}

func newOVNWatchdog(prober sbStateProber) *ovnWatchdog {
	return &ovnWatchdog{
		prober:     prober,
		staleAfter: ovnWatchdogStaleAfter,
		cooldown:   ovnWatchdogCooldown,
	}
}

// evaluate runs one watchdog decision at time now and reports whether it issued a
// reset. Pure of wall-clock (now is injected) so tests drive it deterministically.
// A probe error is treated as unknown — no reset — so a flaky appctl never forces a
// reset on a healthy SB.
func (w *ovnWatchdog) evaluate(ctx context.Context, now time.Time) bool {
	status, err := w.prober.SBConnectionState(ctx)
	if err != nil {
		slog.Warn("ovn-controller watchdog: SB connection-status probe failed", "err", err)
		return false
	}
	if status == "connected" {
		w.nonConnectedSince = time.Time{}
		return false
	}

	if w.nonConnectedSince.IsZero() {
		w.nonConnectedSince = now
	}
	if now.Sub(w.nonConnectedSince) < w.staleAfter {
		return false
	}
	if !w.lastReset.IsZero() && now.Sub(w.lastReset) < w.cooldown {
		return false
	}

	slog.Warn("ovn-controller watchdog: SB wedge detected, resetting SB cluster state",
		"status", status, "stale_for", now.Sub(w.nonConnectedSince).Round(time.Second))
	if err := w.prober.ResetSBClusterState(ctx); err != nil {
		slog.Warn("ovn-controller watchdog: sb-cluster-state-reset failed", "err", err)
		// Still record the attempt so the cooldown throttles retries.
	}
	w.lastReset = now
	// Re-arm the stale timer: give the reset a full staleAfter window to take
	// effect before the next reset is even considered.
	w.nonConnectedSince = time.Time{}
	return true
}

// runOVNControllerWatchdog ticks the watchdog until ctx is cancelled.
func runOVNControllerWatchdog(ctx context.Context, w *ovnWatchdog) {
	ticker := time.NewTicker(ovnWatchdogInterval)
	defer ticker.Stop()
	slog.Info("ovn-controller wedge watchdog started",
		"interval", ovnWatchdogInterval, "stale_after", w.staleAfter, "cooldown", w.cooldown)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.evaluate(ctx, time.Now())
		}
	}
}
