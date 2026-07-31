// rds-agent runs inside a Spinifex RDS instance. It registers the VM with the
// control plane over TLS+SigV4 (never NATS), writes the bootstrap handoff the
// engine's first-boot script consumes, then heartbeats health and polls for
// directives.
//
// It is the only path by which a secret reaches the VM: the master password is
// served once, to an authenticated caller. Static config is read from the
// cloud-init env file /etc/spinifex-rds/agent.env; real env vars override it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mulgadc/spinifex/internal/fipsboot"
)

func main() {
	cfg := loadConfig(defaultEnvFile)

	agent, err := New(cfg)
	if err != nil {
		slog.Error("rds-agent: startup failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A shutdown signal cancels whatever boot retry loop was running; that is a
	// clean stop, not a failure.
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("rds-agent: run failed", "err", err)
		os.Exit(1)
	}
}
