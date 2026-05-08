package vm

import (
	"context"
	"log/slog"
	"time"
)

const (
	// PendingWatchdogInterval is the polling cadence for the stuck-in-pending
	// watchdog. Exported so tests can match production's window without
	// repeating the literal.
	PendingWatchdogInterval = 60 * time.Second
	// PendingWatchdogTimeout is how long an instance may stay in
	// Pending/Provisioning before the watchdog marks it failed.
	PendingWatchdogTimeout = 5 * time.Minute
)

// StartPendingWatchdog spawns a background goroutine that periodically
// scans for instances stuck in Pending/Provisioning beyond
// PendingWatchdogTimeout and marks them failed via Manager.MarkFailed. The
// goroutine exits when ctx is cancelled. Safe to call once per Manager
// (additional calls would duplicate the goroutine).
func (m *Manager) StartPendingWatchdog(ctx context.Context) {
	ticker := time.NewTicker(PendingWatchdogInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.scanAndMarkStuckPending(time.Now())
			}
		}
	}()
}

// scanAndMarkStuckPending runs one pass of the watchdog body with a
// caller-supplied "now". Extracted so tests can drive the body
// deterministically without waiting for the production tick interval.
func (m *Manager) scanAndMarkStuckPending(now time.Time) {
	stuck := m.Filter(func(v *VM) bool {
		return (v.Status == StatePending || v.Status == StateProvisioning) &&
			v.Instance != nil && v.Instance.LaunchTime != nil &&
			now.Sub(*v.Instance.LaunchTime) > PendingWatchdogTimeout
	})

	for _, instance := range stuck {
		slog.Warn("Instance stuck in pending, marking failed",
			"instanceId", instance.ID, "status", instance.Status,
			"elapsed", now.Sub(*instance.Instance.LaunchTime))
		m.MarkFailed(instance, "launch_timeout")
	}
}
