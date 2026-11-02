// Command eks-pod-identity-webhook is the IRSA mutating admission webhook run
// on every EKS control-plane server; the implementation is in
// agents/eks/podidentity. It listens on loopback only and is called by the
// local kube-apiserver.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/podidentity"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := podidentity.ParseServiceConfig(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := podidentity.Run(ctx, cfg, log); err != nil {
		log.Error("pod identity webhook stopped", "error", err)
		os.Exit(1)
	}
}
