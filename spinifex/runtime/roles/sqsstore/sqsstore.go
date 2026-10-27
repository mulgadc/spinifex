// Package sqsstore runs the dedicated NATS JetStream server that holds SQS
// message data. It is a separate server from the control-plane NATS in
// package nats and shares none of its ports, store, accounts or routes.
package sqsstore

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/nats"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

const (
	serviceName = "sqs-store"

	// MinMaxPayload admits a 1 MiB body with 10 attributes, headers and framing.
	MinMaxPayload = 2 * 1024 * 1024

	// MemoryLimitBytes is the soft Go heap limit applied when GOMEMLIMIT is unset.
	MemoryLimitBytes = 512 * 1024 * 1024
)

// Config configures the SQS store server from its own config file.
type Config struct {
	ConfigFile string `json:"config_file"`
	DataDir    string `json:"data_dir"`
}

// Service runs the dedicated SQS JetStream server.
type Service struct {
	Config *Config
}

// New returns the SQS store service. config must be a *Config.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for sqs-store service")
	}
	if cfg.ConfigFile == "" {
		return nil, fmt.Errorf("sqs-store requires a config file")
	}
	return &Service{Config: cfg}, nil
}

// Start runs the server until it shuts down.
func (svc *Service) Start() (int, error) {
	if err := hostprocess.WritePidFileTo(svc.Config.DataDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	if err := launch(svc.Config); err != nil {
		return 0, err
	}
	return os.Getpid(), nil
}

// ValidateOptions rejects a server configuration that would break the
// durability or size contract of the SQS store.
func ValidateOptions(opts *natsserver.Options) error {
	if !opts.JetStream {
		return fmt.Errorf("sqs-store requires jetstream to be enabled")
	}
	if !opts.SyncAlways {
		return fmt.Errorf("sqs-store requires sync_interval: always")
	}
	if opts.MaxPayload < MinMaxPayload {
		return fmt.Errorf("sqs-store max_payload %d is below the %d bytes a full SQS message needs", opts.MaxPayload, MinMaxPayload)
	}
	return nil
}

func launch(cfg *Config) error {
	opts, err := natsserver.ProcessConfigFile(cfg.ConfigFile)
	if err != nil {
		return fmt.Errorf("process sqs-store config: %w", err)
	}
	if err := ValidateOptions(opts); err != nil {
		return err
	}

	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(MemoryLimitBytes)
	}

	reserve := &nats.StoreReserve{Dir: opts.StoreDir, Size: nats.DefaultStoreReserveBytes}
	reserve.Apply()

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return fmt.Errorf("create sqs-store server: %w", err)
	}
	ns.ConfigureLogger()

	if err := natsserver.Run(ns); err != nil {
		return fmt.Errorf("run sqs-store server: %w", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go reserve.Maintain(stop)

	slog.Info("SQS store server started", "server_name", opts.ServerName, "store_dir", opts.StoreDir)
	ns.WaitForShutdown()
	return nil
}
