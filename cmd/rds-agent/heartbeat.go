package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

const (
	servingParameterRecordTimeout = 15 * time.Second
	maxBootstrapFailureBytes      = 512
)

// The beat carries the probe's result, not the agent's own liveness: an agent
// up while the engine is down is what the recovery reconciler must see.
type heartbeater struct {
	cp       controlPlane
	probe    *engineProbe
	recorder servingParameterRecorder
	id       identity
	interval time.Duration

	// Bootstrap and heartbeat run concurrently. Keeping the latest failure here
	// makes a VM stuck before engine startup explain why through its next beat.
	bootstrapFailure atomic.Pointer[string]
}

func newHeartbeater(cp controlPlane, probe *engineProbe, recorder servingParameterRecorder, interval time.Duration) *heartbeater {
	if interval <= 0 {
		interval = handlers_rds.HeartbeatInterval
	}
	return &heartbeater{cp: cp, probe: probe, recorder: recorder, interval: interval}
}

// Called before Run starts and from Run's own goroutine after, so the interval
// is never written concurrently with the loop reading it.
func (h *heartbeater) setInterval(d time.Duration) {
	if d > 0 {
		h.interval = d
	}
}

func (h *heartbeater) setBootstrapFailure(what string, err error) {
	message := what + " failed: " + err.Error()
	if len(message) > maxBootstrapFailureBytes {
		limit := maxBootstrapFailureBytes - len("...")
		for limit > 0 && !utf8.RuneStart(message[limit]) {
			limit--
		}
		message = message[:limit] + "..."
	}
	h.bootstrapFailure.Store(&message)
}

func (h *heartbeater) clearBootstrapFailure() {
	h.bootstrapFailure.Store(nil)
}

func (h *heartbeater) beat(ctx context.Context) {
	health, message := h.probe.Check(ctx)
	if failure := h.bootstrapFailure.Load(); failure != nil {
		if message != "" {
			message += "; "
		}
		message += *failure
	}
	if health == handlers_rds.EngineHealthHealthy && h.recorder != nil {
		// Checking every healthy beat also observes a restart that completed
		// between probes. The recorder skips unchanged and pending-restart sets.
		recordCtx, cancel := context.WithTimeout(ctx, servingParameterRecordTimeout)
		err := h.recorder.RecordServingParameters(recordCtx)
		cancel()
		if err != nil {
			slog.Warn("rds-agent: recording the serving parameters failed", "err", err)
		}
	}

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
