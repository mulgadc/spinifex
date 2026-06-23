package northstar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	nsserver "github.com/mulgadc/northstar/pkg/server"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var serviceName = "northstar"

// Config holds the configuration for the northstar service.
type Config struct {
	// ConfigPath is the path to northstar.toml (written by `spx admin init`).
	ConfigPath string
	// BasePath is the node base dir where the PID file is written.
	BasePath string
	NodeID   int
}

// Service wraps the northstar DNS server library.
type Service struct {
	Config *Config
	server *nsserver.Server
	nc     *nats.Conn
}

// New creates a new northstar service.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for northstar service")
	}
	return &Service{Config: cfg}, nil
}

// Start loads northstar.toml, binds the DNS listeners and zone-sync loop, writes
// the PID file, then blocks until SIGINT/SIGTERM before shutting down.
func (svc *Service) Start() (int, error) {
	if svc.Config.ConfigPath == "" {
		return 0, fmt.Errorf("northstar config path is required (set ConfigPath)")
	}

	serverCfg, err := nsconfig.LoadServerConfig(svc.Config.ConfigPath)
	if err != nil {
		return 0, fmt.Errorf("load northstar config: %w", err)
	}

	server, err := nsserver.NewServer(serverCfg)
	if err != nil {
		return 0, fmt.Errorf("create northstar server: %w", err)
	}
	svc.server = server

	if err := utils.WritePidFileTo(svc.Config.BasePath, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := server.Start(ctx); err != nil {
		slog.Error("Failed to start northstar server", "error", err)
		return 0, err
	}

	svc.subscribeReload(serverCfg.NatsURL)

	<-ctx.Done()

	slog.Info("Shutting down northstar service")
	if svc.nc != nil {
		svc.nc.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Error during northstar shutdown", "error", err)
	}

	return os.Getpid(), nil
}

// subscribeReload connects to NATS (when nats_url is set) and reloads a single
// zone on each fan-out event, so a control-plane record change is served almost
// immediately instead of after the next S3 sync poll. Best-effort: a connect
// failure is logged and the poll remains the backstop.
func (svc *Service) subscribeReload(natsURL string) {
	if natsURL == "" {
		slog.Info("northstar: nats_url not set, relying on S3 poll for zone updates")
		return
	}
	nc, err := nats.Connect(natsURL, nats.Name("northstar-reload"))
	if err != nil {
		slog.Warn("northstar: connect NATS for zone reload", "url", natsURL, "error", err)
		return
	}
	svc.nc = nc

	if _, err := nc.Subscribe(handlers_dns.SubjectZoneReload, func(msg *nats.Msg) {
		var evt handlers_dns.ZoneReload
		if err := json.Unmarshal(msg.Data, &evt); err != nil || evt.Zone == "" {
			return
		}
		if err := svc.server.ReloadZone(evt.Zone); err != nil {
			slog.Warn("northstar: reload zone", "zone", evt.Zone, "error", err)
			return
		}
		slog.Info("northstar: zone reloaded via NATS", "zone", evt.Zone)
	}); err != nil {
		slog.Warn("northstar: subscribe zone reload", "error", err)
		return
	}
	slog.Info("northstar: subscribed to live zone reload", "subject", handlers_dns.SubjectZoneReload)
}

// Stop signals a running northstar service via its PID file.
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BasePath, serviceName)
}

// Status returns the status of the northstar service.
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BasePath, serviceName)
}

// Shutdown gracefully shuts down the in-process server, falling back to Stop.
func (svc *Service) Shutdown() error {
	if svc.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return svc.server.Shutdown(ctx)
	}
	return svc.Stop()
}

// Reload re-reads the zone database without restarting the listeners.
func (svc *Service) Reload() error {
	if svc.server != nil {
		return svc.server.Reload()
	}
	return nil
}
