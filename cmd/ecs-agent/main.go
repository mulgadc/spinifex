// ecs-agent runs inside a Spinifex ECS container instance (a guest VM booted from
// the ECS-AMI, with containerd baked in). It registers the host with the ECS
// scheduler through the AWS gateway over TLS+SigV4 (never NATS), heartbeats by
// re-registering, polls the gateway for task assignments, runs them through
// containerd, and reports state back over the gateway.
//
// Static config (gateway URL, CA, region, cluster, seeded IAM creds, containerd
// socket) is read from the cloud-init env file /etc/spinifex-ecs/agent.env
// (KEY=value); real env vars override it. Host identity (account, instance, AZ)
// comes from IMDS at boot.
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
