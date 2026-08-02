package predastore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mulgadc/predastore/clusterrun"
	"github.com/mulgadc/predastore/pkg/masterkey"
	"github.com/mulgadc/predastore/s3"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"golang.org/x/sync/errgroup"
)

var serviceName = "predastore"

// Config holds the configuration for the predastore service.
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

	// HostID selects which host of the predastore topology this process is.
	// A host owns one socket and one data directory, and runs the nodes
	// pinned to it. Zero runs every node in the topology in this process,
	// which is the single-node deployment and needs no intra-cluster network.
	HostID int

	// Profiling
	PprofEnabled    bool
	PprofOutputPath string
}

// Service wraps the predastore S3 gateway and the cluster runtime beneath it.
type Service struct {
	Config *Config
	server *s3.Server

	// stop cancels the process context Start runs under, so Shutdown can
	// drain the gateway and the nodes together. Guarded because Shutdown is
	// called from a different goroutine than Start.
	mu   sync.Mutex
	stop context.CancelFunc
}

// New creates a new predastore service.
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

// Start starts the predastore service.
func (svc *Service) Start() (int, error) {
	if svc.Config.EncryptionKeyFile == "" {
		return 0, fmt.Errorf("predastore encryption key file is required (set EncryptionKeyFile)")
	}

	if err := utils.WritePidFileTo(svc.Config.BasePath, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	rt, err := svc.buildRuntime()
	if err != nil {
		return 0, err
	}

	server, err := s3.NewServer(
		s3.WithConfigPath(svc.Config.ConfigPath),
		s3.WithAddress(svc.Config.Host, svc.Config.Port),
		s3.WithTLS(svc.Config.TlsCert, svc.Config.TlsKey),
		s3.WithBasePath(svc.Config.BasePath),
		s3.WithDebug(svc.Config.Debug),
		s3.WithPprof(svc.Config.PprofEnabled, svc.Config.PprofOutputPath),
		s3.WithEncryptionKeyFile(svc.Config.EncryptionKeyFile),
		s3.WithPreparedBackend(rt.Backend),
	)
	if err != nil {
		rt.Close()
		slog.Error("Failed to create predastore server", "error", err)
		return 0, err
	}

	// One context for the whole service: a signal, a fatal gateway error or
	// Shutdown all stop the cluster nodes and the gateway together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc.mu.Lock()
	svc.server = server
	svc.stop = stop
	svc.mu.Unlock()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return rt.Run(gctx) })
	g.Go(func() error {
		// Serving before consensus settles would fail writes that would have
		// succeeded a moment later; a timeout degrades rather than aborts,
		// since the state client retries.
		if err := rt.WaitReady(30 * time.Second); err != nil {
			slog.Warn("No predastore leader elected within timeout, serving anyway", "error", err)
		}
		return svc.serve(gctx, server)
	})

	if err := g.Wait(); err != nil {
		slog.Error("Predastore service exited", "error", err)
		return 0, err
	}

	return os.Getpid(), nil
}

// buildRuntime reads the predastore topology and assembles the cluster nodes
// this process is responsible for, returning the backend the S3 gateway runs
// on top of.
func (svc *Service) buildRuntime() (*clusterrun.Runtime, error) {
	cfg := &s3.Config{ConfigPath: svc.Config.ConfigPath, BasePath: svc.Config.BasePath}
	if err := cfg.ReadConfig(); err != nil {
		return nil, fmt.Errorf("read predastore config %s: %w", svc.Config.ConfigPath, err)
	}

	// An unset host means this process is the whole cluster, so it runs every
	// node over the in-process pipe and opens no intra-cluster socket.
	nodeIDs := clusterrun.AllNodeIDs(cfg)
	if svc.Config.HostID > 0 {
		nodeIDs = clusterrun.NodeIDsForHost(cfg, svc.Config.HostID)
		if len(nodeIDs) == 0 {
			return nil, fmt.Errorf("predastore host %d has no nodes in %s", svc.Config.HostID, svc.Config.ConfigPath)
		}
	}

	key, err := masterkey.Load(svc.Config.EncryptionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load predastore master key: %w", err)
	}

	rt, err := clusterrun.Build(cfg, nodeIDs, svc.Config.TlsCert, svc.Config.TlsKey, key)
	if err != nil {
		return nil, fmt.Errorf("build predastore cluster runtime: %w", err)
	}
	return rt, nil
}

// serve runs the S3 gateway until the context is cancelled, then drains it
// within a bounded grace period. A gateway that cannot bind is fatal: leaving
// the cluster running headless would look healthy while serving nothing.
func (svc *Service) serve(ctx context.Context, server *s3.Server) error {
	if err := server.ListenAndServeAsync(); err != nil {
		return fmt.Errorf("start predastore s3 gateway: %w", err)
	}

	select {
	case <-ctx.Done():
	case err := <-server.ServeError():
		return fmt.Errorf("predastore s3 gateway: %w", err)
	}

	slog.Info("Shutting down predastore service")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down predastore s3 gateway: %w", err)
	}
	return nil
}

// Stop stops the predastore service.
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BasePath, serviceName)
}

// Status returns the status of the predastore service.
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BasePath, serviceName)
}

// Shutdown gracefully shuts down the predastore service. When this process is
// the one running the cluster it cancels the service context, which drains the
// gateway and every local node; otherwise it signals the running process.
func (svc *Service) Shutdown() error {
	svc.mu.Lock()
	stop := svc.stop
	svc.mu.Unlock()

	if stop != nil {
		stop()
		return nil
	}
	return svc.Stop()
}

// Reload reloads the predastore service configuration.
func (svc *Service) Reload() error {
	return nil
}
