package main

import (
	"context"
	"log/slog"
	"time"
)

// register registers the VM and adopts what the control plane answers with: the
// authoritative DB instance identifier and the heartbeat cadence. Both stay
// control-plane-owned rather than being guessed in the guest — the identifier
// because the gateway resolves it from the caller's credentials, the cadence
// because the staleness window it feeds is the control plane's to set.
//
// Registration is idempotent, so an agent restart re-registers rather than
// needing to know whether it had registered before.
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
