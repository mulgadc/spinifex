// Package qmpcollector is the per-node guest-metrics producer (Goanna Phase 1):
// it discovers running VMs via their qmp-telemetry-*.json sidecar files, polls
// each VM's dedicated telemetry QMP socket (never the manager's control
// socket) plus host tap counters, and publishes CloudWatch-mappable series to
// NATS metrics.ec2.<instance-id>. A bridge goroutine forwards the series to
// the local OTLP receiver for the operator plane until Goanna consumes them.
package qmpcollector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

var serviceName = "qmp-collector"

// Config holds the qmp-collector service configuration.
type Config struct {
	// NatsHost is the NATS server address (host:port).
	NatsHost string
	// NatsToken is the NATS authentication token.
	NatsToken string
	// NatsCACert is the path to the CA certificate for NATS TLS.
	NatsCACert string
	// BaseDir is the base directory for PID files and state.
	BaseDir string
	// NodeName identifies this node in the published series.
	NodeName string

	// RuntimeDir overrides where qmp-telemetry-* sockets and metadata live
	// (default utils.RuntimeDir()); ProcRoot / SysRoot override /proc and
	// /sys. Test injection points.
	RuntimeDir string
	ProcRoot   string
	SysRoot    string
	// DiscoverInterval is how often the metadata dir is rescanned (default 15s).
	DiscoverInterval time.Duration
}

// Service supervises the collector, mirroring the vpcd/viperblockd wrappers.
type Service struct {
	Config *Config
}

// New creates a new qmp-collector Service.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for qmp-collector service")
	}
	return &Service{Config: cfg}, nil
}

// Start runs the collector until SIGINT/SIGTERM.
func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	if err := launchService(svc.Config); err != nil {
		slog.Error("Failed to launch qmp-collector service", "err", err)
		return 0, err
	}
	return os.Getpid(), nil
}

// Stop stops the qmp-collector service.
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BaseDir, serviceName)
}

// Status returns the qmp-collector service status.
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BaseDir, serviceName)
}

// Shutdown gracefully shuts down the qmp-collector service.
func (svc *Service) Shutdown() error {
	return svc.Stop()
}

// Reload reloads the qmp-collector service configuration.
func (svc *Service) Reload() error {
	return nil
}

func launchService(cfg *Config) error {
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = utils.RuntimeDir()
	}
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.SysRoot == "" {
		cfg.SysRoot = "/sys"
	}
	if cfg.DiscoverInterval <= 0 {
		cfg.DiscoverInterval = 15 * time.Second
	}

	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}
	defer nc.Close()

	stopBridge, err := startBridge(nc)
	if err != nil {
		return fmt.Errorf("start metrics bridge: %w", err)
	}
	defer stopBridge()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Info("qmp-collector: shutting down", "signal", sig.String())
		cancel()
	}()

	slog.Info("qmp-collector: started",
		"runtime_dir", cfg.RuntimeDir, "node", cfg.NodeName)
	newCollector(cfg, nc).run(ctx)
	return nil
}
