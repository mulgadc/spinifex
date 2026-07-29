package main

import (
	"context"
	"log/slog"
	"time"
)

// Adopts the authoritative DB instance identifier and heartbeat cadence from
// the response. Idempotent, so a restart re-registers unconditionally.
func (a *Agent) register(ctx context.Context) error {
	return retry(ctx, "register", func(ctx context.Context) error {
		out, err := a.cp.Register(ctx, a.id)
		if err != nil {
			return err
		}
		if out.DBInstanceIdentifier != "" {
			a.id.DBInstanceIdentifier = out.DBInstanceIdentifier
		}
		a.hb.setInterval(time.Duration(out.HeartbeatIntervalSeconds) * time.Second)

		slog.Info("rds-agent: registered",
			"dbInstanceIdentifier", a.id.DBInstanceIdentifier, "heartbeat", a.hb.interval)
		return nil
	})
}
