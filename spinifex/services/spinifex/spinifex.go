// Package spinifex runs the per-node Spinifex daemon as a Spinifex service.
package spinifex

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

var serviceName = "spinifex"

// Service runs this node's Spinifex daemon from the cluster config. ConfigPath
// is the spinifex.toml the daemon was loaded from, set via SetConfigPath.
type Service struct {
	Config     *config.ClusterConfig
	ConfigPath string
}

// New returns the daemon service. cfg must be a *config.ClusterConfig;
// anything else errors.
func New(cfg any) (svc *Service, err error) {
	c, ok := cfg.(*config.ClusterConfig)
	if !ok {
		return nil, fmt.Errorf("invalid config type for spinifex service")
	}
	svc = &Service{
		Config: c,
	}
	return svc, nil
}

func (svc *Service) SetConfigPath(path string) {
	svc.ConfigPath = path
}

func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.NodeBaseDir(), serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config, svc.ConfigPath)
	if err != nil {
		return 0, err
	}

	return os.Getpid(), nil
}

func launchService(config *config.ClusterConfig, configPath string) (err error) {
	d, err := daemon.NewDaemon(config)
	if err != nil {
		return fmt.Errorf("create daemon: %w", err)
	}
	d.SetConfigPath(configPath)
	slog.Info("Starting Spinifex daemon ...")
	err = d.Start()

	if err != nil {
		slog.Warn("Failed to start Spinifex daemon", "err", err)
		return err
	}

	return nil
}
