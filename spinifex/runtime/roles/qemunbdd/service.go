package qemunbdd

import (
	"context"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/netaddr"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// serviceName roots this daemon's PID file: baseDir/qemunbd.pid.
var serviceName = "qemunbd"

// Config is the qemunbdd service's startup configuration, mirroring the
// Viperblock adapter's NATS and base-directory fields. NodeName scopes the
// natsserve PublishVolume/UnpublishVolume subjects to this node.
type Config struct {
	BaseDir    string
	NatsHost   string
	NatsToken  string
	NatsCACert string
	NodeName   string
	Debug      bool
}

// Service runs the qcow2/qemu-nbd EBSProvider behind natsserve. It and
// viperblockd both answer ebs.provider.v1.*, so only one of the two may be
// pointed at the same NATS cluster at a time.
type Service struct {
	Config *Config
}

// New returns the qemunbdd service. config must be a *Config; anything else
// errors.
func New(config any) (svc *Service, err error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for qemunbdd service")
	}
	return &Service{Config: cfg}, nil
}

func (svc *Service) Start() (int, error) {
	if err := hostprocess.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	if err := launchService(svc.Config); err != nil {
		slog.Error("Failed to launch service", "err", err)
		return 0, err
	}
	return os.Getpid(), nil
}

// launchService connects to NATS, roots a qcow2 provider at cfg.BaseDir and
// serves ebs.provider.v1.* until SIGINT/SIGTERM, then unsubscribes. It blocks
// for the life of the process.
func launchService(cfg *Config) error {
	nc, err := natsmsg.ConnectNATSWithRetry(netaddr.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Close()

	provider, err := NewProvider(cfg.BaseDir)
	if err != nil {
		return fmt.Errorf("construct qemunbd provider: %w", err)
	}

	stop, err := natsserve.Serve(context.Background(), nc, provider, natsserve.Options{NodeID: cfg.NodeName})
	if err != nil {
		return fmt.Errorf("serve ebs.provider.v1: %w", err)
	}

	slog.Info("qemunbdd: waiting for EBS provider events", "node", cfg.NodeName)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	slog.Info("qemunbdd: shutting down gracefully...")
	stop()

	return nil
}
