package main

import (
	"context"
	"log/slog"
	"time"
)

// heartbeater keeps the container instance alive by periodically re-registering
// it through the gateway. Real ECS folds liveness into ACS; v1 folds it into an
// idempotent RegisterContainerInstance, which refreshes the instance's LastSeen
// so the scheduler can tell live instances from dead ones (no bus.Heartbeat).
type heartbeater struct {
	cp       controlPlane
	id       identity
	interval time.Duration
}

func newHeartbeater(cp controlPlane, id identity, interval time.Duration) *heartbeater {
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	return &heartbeater{cp: cp, id: id, interval: interval}
}

// beat re-registers the instance once, refreshing its LastSeen.
func (h *heartbeater) beat() error {
	return h.cp.Register(h.id)
}

// Run beats every interval until ctx is cancelled. A failed beat is logged and
// retried on the next tick rather than killing the loop.
func (h *heartbeater) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.beat(); err != nil {
				slog.Warn("ecs-agent: heartbeat (re-register) failed", "err", err)
			}
		}
	}
}
