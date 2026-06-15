package nats

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats-server/v2/server"
)

var serviceName = "nats"

type Config struct {
	ConfigFile string `json:"config_file"`
	Port       int    `json:"port"`
	Host       string `json:"host"`
	Debug      bool   `json:"debug"`
	LogFile    string `json:"log_file"`
	DataDir    string `json:"data_dir"`
	JetStream  bool   `json:"jetstream"`
}

type Service struct {
	Config *Config
}

func New(config any) (svc *Service, err error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for nats service")
	}
	svc = &Service{
		Config: cfg,
	}
	return svc, nil
}

func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.DataDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config)
	if err != nil {
		return 0, err
	}
	return os.Getpid(), nil
}

func (svc *Service) Stop() (err error) {
	return utils.StopProcessAt(svc.Config.DataDir, serviceName)
}

func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.DataDir, serviceName)
}

func (svc *Service) Shutdown() (err error) {
	return svc.Stop()
}

func (svc *Service) Reload() (err error) {
	return nil
}

func launchService(config *Config) (err error) {
	// Create proper server options
	var opts *server.Options

	// If configFile set use, otherwise set defaults
	if config.ConfigFile != "" {
		opts, err = server.ProcessConfigFile(config.ConfigFile)

		if err != nil {
			slog.Error("Failed to process NATS config file", "err", err)
			return err
		}
	} else {
		opts = &server.Options{
			ConfigFile: config.ConfigFile,
			Port:       config.Port,
			Host:       config.Host,
			Debug:      config.Debug,
			LogFile:    config.LogFile,
			JetStream:  config.JetStream,
			StoreDir:   config.DataDir,
		}

		// Set defaults if not provided
		if opts.Port == 0 {
			opts.Port = 4222
		}
		if opts.Host == "" {
			opts.Host = "0.0.0.0"
		}
	}

	slog.Debug("NATS server options", "opts", opts)

	// Initialize new server with options
	ns, err := server.NewServer(opts)
	if err != nil {
		slog.Error("Failed to create NATS server", "err", err)
		return err
	}

	ns.ConfigureLogger()

	if err := server.Run(ns); err != nil {
		server.PrintAndDie(err.Error())
	}

	ns.WaitForShutdown()

	return nil
}
