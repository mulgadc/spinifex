package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// heartbeater periodically emits instance-heartbeat messages so the scheduler
// can tell live instances from dead ones.
type heartbeater struct {
	pub      publisher
	id       identity
	interval time.Duration
	// runningTasks reports the current running-task count; nil means zero.
	runningTasks func() int
}

func newHeartbeater(pub publisher, id identity, interval time.Duration, running func() int) *heartbeater {
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	return &heartbeater{pub: pub, id: id, interval: interval, runningTasks: running}
}

// beat publishes a single Heartbeat message.
func (h *heartbeater) beat() error {
	n := 0
	if h.runningTasks != nil {
		n = h.runningTasks()
	}
	msg := bus.Heartbeat{
		AccountID:    h.id.AccountID,
		ClusterName:  h.id.ClusterName,
		InstanceID:   h.id.InstanceID,
		Status:       bus.StatusActive,
		RunningTasks: n,
		SentAt:       time.Now().UTC(),
	}
	data, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	subj := bus.HeartbeatSubject(h.id.AccountID, h.id.ClusterName, h.id.InstanceID)
	if err := h.pub.Publish(subj, data); err != nil {
		return fmt.Errorf("publish heartbeat %s: %w", subj, err)
	}
	return nil
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
				slog.Warn("ecs-agent: heartbeat failed", "err", err)
			}
		}
	}
}
