package predastore

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/predastore/pkg/masterkey"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

var serviceName = "predastore"

// Config is what spinifex.toml and the service flags contribute. Everything
// else — the cluster topology, the buckets, the credentials — comes from the
// predastore configuration file at ConfigPath.
type Config struct {
	ConfigPath string
	Port       int
	// Host is where the S3 API listens, and only that: the cluster plane binds
	// the host's own address from the predastore configuration file.
	Host    string
	TlsCert string
	TlsKey  string

	// BasePath is where this service keeps its pid file. Predastore itself has
	// no base path — its directories come from the configuration file — but
	// Stop and Status find the running process through this one.
	BasePath string

	// EncryptionKeyFile is the path to this node's 32-byte AES-256 master
	// key for predastore at-rest encryption. Each node has its own key;
	// fragments are only ever opened on the node that sealed them.
	EncryptionKeyFile string

	// HostID selects which [[host]] of the predastore topology this process
	// runs: the S3 gate pinned to it and every cluster node beside it. It must
	// name a host the configuration file declares.
	HostID int
}

// Service runs one predastore host in this process.
type Service struct {
	Config *Config

	// stop cancels the process context Start runs under, so Shutdown can
	// drain the gate and the nodes together. Guarded because Shutdown is
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

// Start serves this host until the service context is cancelled, blocking for
// as long as it serves.
func (svc *Service) Start() (int, error) {
	if svc.Config.EncryptionKeyFile == "" {
		return 0, fmt.Errorf("predastore encryption key file is required (set EncryptionKeyFile)")
	}

	if err := utils.WritePidFileTo(svc.Config.BasePath, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	cfg, err := pds.LoadConfig(svc.Config.ConfigPath)
	if err != nil {
		return 0, fmt.Errorf("read predastore config %s: %w", svc.Config.ConfigPath, err)
	}
	hostID, err := svc.mergeHost(cfg)
	if err != nil {
		return 0, err
	}

	key, err := masterkey.Load(svc.Config.EncryptionKeyFile)
	if err != nil {
		return 0, fmt.Errorf("load predastore master key: %w", err)
	}

	// One context for the whole service: a signal, a fatal node error or
	// Shutdown stops the gate and every local node together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc.mu.Lock()
	svc.stop = stop
	svc.mu.Unlock()

	if err := pds.Run(ctx, pds.Options{
		Config:    cfg,
		HostID:    hostID,
		MasterKey: key,
	}); err != nil {
		slog.Error("Predastore service exited", "error", err)
		return 0, err
	}
	return os.Getpid(), nil
}

// mergeHost settles the host-local fields spinifex.toml owns into the parsed
// configuration, then rechecks the merged tree: a flag can supply an address or
// a port the file never had, and the collision checks only mean something once
// those are in place. It returns the host this process runs.
func (svc *Service) mergeHost(cfg *pds.Config) (pds.HostID, error) {
	if svc.Config.HostID <= 0 {
		return 0, fmt.Errorf("predastore host id is required and must name a [[host]] in %s", svc.Config.ConfigPath)
	}
	hostID := pds.HostID(svc.Config.HostID)

	var host *pds.HostConfig
	for i := range cfg.Hosts {
		if cfg.Hosts[i].ID == hostID {
			host = &cfg.Hosts[i]
			break
		}
	}
	if host == nil {
		return 0, fmt.Errorf("predastore host %d is not in %s", svc.Config.HostID, svc.Config.ConfigPath)
	}

	// host.BindAddr is left as the file has it: it is the cluster plane, there
	// is no flag for it, and predastore falls back to the host's dial address
	// when it is empty.
	host.TLSCert = cmp.Or(svc.Config.TlsCert, host.TLSCert)
	host.TLSKey = cmp.Or(svc.Config.TlsKey, host.TLSKey)

	// The endpoint spinifex.toml advertises is this host's gate, found by role:
	// nothing fixes its position among the host's nodes.
	gate := -1
	for i := range host.Nodes {
		if host.Nodes[i].Role == pds.RoleGate {
			gate = i
			break
		}
	}
	if gate < 0 {
		return 0, fmt.Errorf("predastore host %d runs no gate node in %s", svc.Config.HostID, svc.Config.ConfigPath)
	}
	// The address spinifex.toml carries is the S3 endpoint, which is the gate's
	// alone. Settling it on the host would put raft and blob traffic on the
	// public interface with it, which is the one place they must never be.
	host.Nodes[gate].BindAddr = cmp.Or(svc.Config.Host, host.Nodes[gate].BindAddr)
	host.Nodes[gate].Port = cmp.Or(svc.Config.Port, host.Nodes[gate].Port)

	return hostID, cfg.Validate()
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
// the one running the host it cancels the service context, which drains the
// gate and every local node; otherwise it signals the running process.
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
