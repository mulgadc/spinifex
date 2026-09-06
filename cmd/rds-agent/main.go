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

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"
)

func main() {
	cfg := loadConfig(defaultEnvFile)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Retried rather than fatal: construction resolves the IMDS credential chain,
	// so a metadata service that is not answering yet would otherwise end the
	// process with the engine still down and nothing left to bring it back.
	var agent *Agent
	err := retry(ctx, "startup", func(context.Context) error {
		var err error
		agent, err = New(cfg)
		return err
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Error("rds-agent: startup failed", "err", err)
		os.Exit(1)
	}

	// A shutdown signal cancels whatever boot retry loop was running; that is a
	// clean stop, not a failure.
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("rds-agent: run failed", "err", err)
		os.Exit(1)
	}
}
