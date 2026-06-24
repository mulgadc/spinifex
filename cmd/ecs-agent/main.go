// ecs-agent runs inside a Spinifex ECS container instance (a guest VM booted from
// the ECS-AMI, with containerd baked in). It registers the host with the ECS
// scheduler over the Layer-2 NATS bus, heartbeats while alive, and pulls task
// images from the internal ECR registry through containerd. Task assignment and
// container lifecycle land in Sprint 4e.
//
// Static config (gateway URL, CA, region, cluster, NATS, containerd socket) is
// read from the cloud-init env file /etc/spinifex-ecs/agent.env (KEY=value);
// real env vars override it. Host identity (account, instance, AZ) comes from
// IMDS at boot.
package main

import (
	"context"
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
