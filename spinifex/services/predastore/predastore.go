package predastore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mulgadc/predastore/s3"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

var serviceName = "predastore"

// Config holds the configuration for the predastore service
type Config struct {
	ConfigPath string
	Port       int
	Host       string
	Debug      bool
	BasePath   string
	TlsCert    string
	TlsKey     string

	// EncryptionKeyFile is the path to this node's 32-byte AES-256 master
	// key for predastore at-rest encryption. Each node has its own key;
	// fragments are only ever opened on the node that sealed them.
	EncryptionKeyFile string

	NodeID int

	// Profiling
	PprofEnabled    bool
	PprofOutputPath string
}

// Service wraps the predastore S3 server
type Service struct {
	Config *Config
	server *s3.Server
}

// New creates a new predastore service
func New(config any) (svc *Service, err error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for predastore service")
	}
	svc = &Service{
		Config: cfg,
	}
	return svc, nil
}

// Start starts the predastore service
func (svc *Service) Start() (int, error) {
	if svc.Config.EncryptionKeyFile == "" {
		return 0, fmt.Errorf("predastore encryption key file is required (set EncryptionKeyFile)")
	}

	if err := utils.WritePidFileTo(svc.Config.BasePath, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	server, err := s3.NewServer(
		s3.WithConfigPath(svc.Config.ConfigPath),
		s3.WithAddress(svc.Config.Host, svc.Config.Port),
		s3.WithTLS(svc.Config.TlsCert, svc.Config.TlsKey),
		s3.WithBasePath(svc.Config.BasePath),
		s3.WithDebug(svc.Config.Debug),
		s3.WithNodeID(svc.Config.NodeID),
		s3.WithPprof(svc.Config.PprofEnabled, svc.Config.PprofOutputPath),
		s3.WithEncryptionKeyFile(svc.Config.EncryptionKeyFile),
	)
	if err != nil {
		slog.Error("Failed to create predastore server", "error", err)
		return 0, err
	}

	svc.server = server

	// Start server asynchronously
	if err := server.ListenAndServeAsync(); err != nil {
		slog.Error("Failed to start predastore server", "error", err)
		return 0, err
	}

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("Shutting down predastore service")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		slog.Error("Error during shutdown", "error", err)
	}

	return os.Getpid(), nil
}

// Stop stops the predastore service
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BasePath, serviceName)
}

// Status returns the status of the predastore service
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BasePath, serviceName)
}

// Shutdown gracefully shuts down the predastore service
func (svc *Service) Shutdown() error {
	if svc.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return svc.server.Shutdown(ctx)
	}
	return svc.Stop()
}

// Reload reloads the predastore service configuration
func (svc *Service) Reload() error {
	return nil
}
