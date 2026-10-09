// Package viperblockd runs the Viperblock EBS role: process/role lifecycle
// and the NATS subscription wiring that starts and stops the Viperblock
// adapter (providers/ebs/viperblock).
package viperblockd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/netaddr"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/viperblock"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

var serviceName = "viperblock"

// Service runs viperblockd: it supervises the Viperblock adapter's process
// lifecycle and starts and stops its NATS subscriptions.
type Service struct {
	Config *viperblock.Config
}

// New returns the viperblockd service. config must be a *viperblock.Config;
// anything else errors.
func New(config any) (svc *Service, err error) {
	cfg, ok := config.(*viperblock.Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for viperblockd service")
	}
	svc = &Service{
		Config: cfg,
	}

	return svc, nil
}

func (svc *Service) Start() (int, error) {
	if err := hostprocess.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config)

	if err != nil {
		slog.Error("Failed to launch service", "err", err)
		return 0, err
	}

	return os.Getpid(), nil
}

// launchService connects to NATS, hands the connection to the adapter to run
// its startup sequence (encryption key, lease store, mount recovery, subject
// registration), then blocks until SIGINT/SIGTERM before shutting the
// adapter down.
func launchService(cfg *viperblock.Config) error {
	nc, err := natsmsg.ConnectNATSWithRetry(netaddr.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}

	if err := cfg.Run(nc); err != nil {
		return err
	}

	// Create a channel to receive shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	<-sigChan
	slog.Info("Shutting down gracefully...")

	nc.Close()
	cfg.Shutdown()

	return nil
}
