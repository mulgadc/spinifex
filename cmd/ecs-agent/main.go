// Command ecs-agent runs inside a Spinifex ECS container instance; the
// implementation is in agents/ecs/agent. This binary owns process lifecycle:
// signal handling, startup and run failure logging, and the exit code.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	ecsagent "github.com/mulgadc/spinifex/spinifex/agents/ecs/agent"
)

// version is the agent build version, reported in the register message.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := ecsagent.LoadConfig(ecsagent.DefaultEnvFile)
	cfg.AgentVersion = version

	agent, err := ecsagent.New(cfg)
	if err != nil {
		slog.Error("ecs-agent: startup failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx); err != nil {
		slog.Error("ecs-agent: run failed", "err", err)
		os.Exit(1)
	}
}
