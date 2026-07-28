package main

import (
	"context"
	"log/slog"
	"time"
)

// register registers the VM and adopts what the control plane answers with: the
// authoritative DB instance identifier and the heartbeat cadence. It is
// idempotent, so a restart re-registers rather than tracking whether it had.
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
