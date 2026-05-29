package awsgw

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/predastore/ratelimit"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
)

var serviceName = "awsgw"

// Version and Commit are set by the cmd package before Start() to pass
// build-time ldflags to the gateway without creating an import cycle.
var (
	version = "dev"
	commit  = "unknown"
)

// SetBuildInfo sets the build-time version and commit for the gateway.
// Call before Start().
func SetBuildInfo(v, c string) {
	version = v
	commit = c
}

type Service struct {
	Config *config.ClusterConfig
}

func New(cfg any) (svc *Service, err error) {
	c, ok := cfg.(*config.ClusterConfig)
	if !ok {
		return nil, fmt.Errorf("invalid config type for awsgw service")
	}
	svc = &Service{
		Config: c,
	}
	return svc, nil
}

func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.NodeBaseDir(), serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config)
	if err != nil {
		return 0, err
	}

	return os.Getpid(), nil
}

func (svc *Service) Stop() (err error) {
	return utils.StopProcessAt(svc.Config.NodeBaseDir(), serviceName)
}

func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.NodeBaseDir(), serviceName)
}

func (svc *Service) Shutdown() (err error) {
	return svc.Stop()
}

func (svc *Service) Reload() (err error) {
	return nil
}

// awsgwTOML is the top-level structure of awsgw.toml used to extract the
// ratelimit section. Other fields are parsed elsewhere (e.g. region, debug).
type awsgwTOML struct {
	Ratelimit ratelimit.Config `toml:"ratelimit"`
}

// loadThrottleConfig parses the [ratelimit] section from the awsgw TOML config.
func loadThrottleConfig(path string) (ratelimit.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ratelimit.Config{}, fmt.Errorf("read awsgw config %s: %w", path, err)
	}
	var cfg awsgwTOML
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return ratelimit.Config{}, fmt.Errorf("parse awsgw config %s: %w", path, err)
	}
	return cfg.Ratelimit, nil
}

func launchService(config *config.ClusterConfig) error {
	nodeConfig := config.Nodes[config.Node]

	// Connect to NATS for service communication. On concurrent startup the
	// local NATS server may not be listening yet, so retry with backoff.
	natsConn, err := utils.ConnectNATSWithRetry(admin.DialTarget(nodeConfig.NATS.Host), nodeConfig.NATS.ACL.Token, nodeConfig.NATS.CACert)
	if err != nil {
		return err
	}
	defer natsConn.Close()

	// Append Base dir if config has no leading path
	if nodeConfig.BaseDir != "" && !strings.HasPrefix(nodeConfig.AWSGW.Config, "/") {
		nodeConfig.AWSGW.Config = fmt.Sprintf("%s/%s", nodeConfig.BaseDir, nodeConfig.AWSGW.Config)
	}

	// Load IAM master key from disk (required for all authenticated requests)
	masterKeyPath := filepath.Join(nodeConfig.BaseDir, "config", "master.key")
	masterKey, err := handlers_iam.LoadMasterKey(masterKeyPath)
	if err != nil {
		return fmt.Errorf("load IAM master key from %s: %w", masterKeyPath, err)
	}

	// Initialize IAM service with NATS KV backend (required for auth).
	// On multi-node clusters, JetStream KV requires cluster quorum which may
	// not be available yet if nodes start concurrently. Retry with backoff.
	iamService, err := initIAMService(natsConn, masterKey, len(config.Nodes))
	if err != nil {
		return fmt.Errorf("initialize IAM service: %w", err)
	}

	// STS service shares the IAM master key (single envelope for at-rest
	// secrets + session-token HMACs) and resolves roles via IAMService.
	stsService, err := handlers_sts.NewSTSServiceImpl(natsConn, iamService, masterKey, len(config.Nodes))
	if err != nil {
		return fmt.Errorf("initialize STS service: %w", err)
	}

	// Janitor sweeps expired session credentials. Bound to the process
	// lifetime — the server below blocks until exit, so cancelling on return
	// is sufficient to let the goroutine drain.
	janitorCtx, cancelJanitor := context.WithCancel(context.Background())
	defer cancelJanitor()
	go stsService.RunJanitor(janitorCtx)

	// First boot: consume bootstrap.json → seed IAM users into NATS KV → delete file.
	// Check data directory first (production: /var/lib/spinifex/awsgw/), then
	// awsgw subdir (dev: ~/spinifex/awsgw/), then legacy config dir.
	bootstrapPath := findBootstrapFile(nodeConfig.BaseDir)
	data, err := handlers_iam.LoadBootstrapData(bootstrapPath)
	switch {
	case err == nil:
		slog.Info("Bootstrap file found, seeding IAM users")
		if err := iamService.SeedBootstrap(data); err != nil {
			return fmt.Errorf("seed bootstrap from bootstrap.json: %w", err)
		}
		if err := os.Remove(bootstrapPath); err != nil {
			slog.Warn("Failed to delete bootstrap file", "path", bootstrapPath, "err", err)
		} else {
			slog.Info("Bootstrap complete, bootstrap.json deleted")
		}
	case os.IsNotExist(err):
		// No bootstrap file — normal after first boot
	default:
		return fmt.Errorf("load bootstrap from %s: %w", bootstrapPath, err)
	}

	// Load API throttle config from awsgw.toml [ratelimit] section.
	awsgwTomlPath := filepath.Join(nodeConfig.BaseDir, "config", "awsgw", "awsgw.toml")
	throttleCfg, err := loadThrottleConfig(awsgwTomlPath)
	if err != nil {
		slog.Warn("Failed to load throttle config, throttling disabled", "err", err)
	}

	// Create gateway with NATS connection
	gw := gateway.GatewayConfig{
		Debug:          nodeConfig.AWSGW.Debug,
		DisableLogging: false,
		NATSConn:       natsConn,
		Config:         nodeConfig.AWSGW.Config,
		ExpectedNodes:  len(config.Nodes),
		Region:         nodeConfig.Region,
		AZ:             nodeConfig.AZ,
		IAMService:     iamService,
		STSService:     stsService,
		Version:        version,
		Commit:         commit,
	}

	if throttleCfg.Enabled {
		gw.Throttler = ratelimit.New(throttleCfg)
		defer gw.Throttler.Stop()
	}

	handler := gw.SetupRoutes()

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(nodeConfig.AWSGW.TLSCert, nodeConfig.AWSGW.TLSKey)
	if err != nil {
		return fmt.Errorf("load TLS cert: %w", err)
	}

	server := &http.Server{
		Addr:              nodeConfig.AWSGW.Host,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates:     []tls.Certificate{cert},
			NextProtos:       []string{"h2", "http/1.1"},
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: tlsconfig.Curves,
		},
	}

	slog.Info("AWS Gateway listening", "addr", nodeConfig.AWSGW.Host)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		slog.Error("Failed to start TLS listener", "err", err)
		os.Exit(1)
	}

	return nil
}

// initIAMService initializes the IAM service with retry/backoff. On multi-node
// clusters, JetStream requires NATS cluster quorum before KV buckets can be
// created. This retries for up to 5 minutes to allow late-joining nodes.
func initIAMService(natsConn *nats.Conn, masterKey []byte, clusterSize int) (*handlers_iam.IAMServiceImpl, error) {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		svc, err := handlers_iam.NewIAMServiceImpl(natsConn, masterKey, clusterSize)
		if err == nil {
			if attempt > 1 {
				slog.Info("IAM service initialized after retry", "attempts", attempt, "elapsed", time.Since(start).Round(time.Second))
			}
			return svc, nil
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			return nil, fmt.Errorf("IAM service unavailable after %s (%d attempts): %w", elapsed.Round(time.Second), attempt, err)
		}

		slog.Warn("IAM service not ready (waiting for JetStream cluster quorum)", "error", err, "attempt", attempt, "elapsed", elapsed.Round(time.Second), "retryIn", retryDelay)
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}

// findBootstrapFile returns the path to bootstrap.json, checking the data
// directory first (production), then the awsgw subdir (dev mode), then the
// legacy config dir. Returns the first path that exists, or the primary
// location if none exist (so the caller gets a clean "not found" error).
func findBootstrapFile(baseDir string) string {
	candidates := []string{
		filepath.Join(baseDir, "bootstrap.json"),
		filepath.Join(baseDir, "awsgw", "bootstrap.json"),
		filepath.Join(baseDir, "config", "bootstrap.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}
