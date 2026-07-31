package main

import (
	"context"
	"log/slog"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The beat carries the probe's result, not the agent's own liveness: an agent
// up while the engine is down is what the recovery reconciler must see.
type heartbeater struct {
	cp       controlPlane
	probe    *engineProbe
	id       identity
	interval time.Duration
}

func newHeartbeater(cp controlPlane, probe *engineProbe, interval time.Duration) *heartbeater {
	if interval <= 0 {
		interval = handlers_rds.HeartbeatInterval
	}
	return &heartbeater{cp: cp, probe: probe, interval: interval}
}

// Called before Run starts and from Run's own goroutine after, so the interval
// is never written concurrently with the loop reading it.
func (h *heartbeater) setInterval(d time.Duration) {
	if d > 0 {
		h.interval = d
	}
}

func (h *heartbeater) beat(ctx context.Context) {
	health, message := h.probe.Check(ctx)

	out, err := h.cp.SubmitState(ctx, h.id, health, message)
	if err != nil {
		// Not escalated: the control plane already treats a missing heartbeat as
		// staleness, and the next tick retries.
		slog.Warn("rds-agent: heartbeat failed", "health", health, "err", err)
		return
	}
	slog.Debug("rds-agent: heartbeat", "health", health, "persisted", out.Persisted)
	h.setInterval(time.Duration(out.HeartbeatIntervalSeconds) * time.Second)
}

// A timer rather than a ticker, so a cadence change takes effect on the next
// beat, not at a restart.
func (h *heartbeater) Run(ctx context.Context) {
	timer := time.NewTimer(h.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.beat(ctx)
			timer.Reset(h.interval)
		}
	}
}
