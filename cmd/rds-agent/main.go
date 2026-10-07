// Command rds-agent runs inside a Spinifex RDS instance; the implementation is
// in agents/rds/agent. This binary owns process lifecycle: signal handling,
// treating a cancelled startup as a clean stop, failure logging and the exit
// code.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	rdsagent "github.com/mulgadc/spinifex/spinifex/agents/rds/agent"
)

// version is the agent build version, reported at registration.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := rdsagent.LoadConfig(rdsagent.DefaultEnvFile)
	cfg.AgentVersion = version

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	agent, err := rdsagent.Start(ctx, cfg)
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
