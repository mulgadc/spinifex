// rds-agent runs inside a Spinifex RDS instance (a guest VM booted from an
// engine AMI such as spinifex-rds-postgres). It registers the VM with the RDS
// control plane through the AWS gateway over TLS+SigV4 — never NATS — fetches
// the instance's bootstrap configuration and writes the handoff the engine's
// first-boot script consumes, then heartbeats engine health and polls the
// gateway for control-plane directives.
//
// It is the only path by which a secret reaches the VM: the master password is
// served once, to an authenticated caller, in the bootstrap response. Static
// config (gateway URL, CA, region) is read from the cloud-init env file
// /etc/spinifex-rds/agent.env (KEY=value); real env vars override it.
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

	// A shutdown signal during the boot sequence cancels whatever retry loop was
	// running; that is a clean stop, not a failure to report as one.
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("rds-agent: run failed", "err", err)
		os.Exit(1)
	}
}
