package main

import (
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/internal/guestenv"
)

const (
	defaultEnvFile    = "/etc/spinifex-rds/agent.env"
	defaultGatewayCA  = "/etc/spinifex-rds/gateway-ca.pem"
	defaultHandoffDir = "/run/spinifex-rds"
	defaultEngineHost = "127.0.0.1"
	defaultEnginePort = 5432
	defaultPGIsReady  = "pg_isready"
	// The long-poll window the agent asks the gateway to hold a request open
	// for. The gateway caps it at 20s.
	defaultPollWait = 20 * time.Second
)

// Static settings delivered per-instance by cloud-init. It carries no secrets:
// IMDS is readable by anything in the guest, so the master password only
// arrives via GetDBBootstrapConfig.
type config struct {
	GatewayURL string
	GatewayCA  string
	Region     string
	// Optional — the gateway resolves the instance from the caller's
	// credentials. When set it is sent, so a mis-provisioned VM is rejected.
	DBInstanceIdentifier string
	// What this image actually ships. Empty leaves the control plane's recorded
	// version alone rather than clearing it.
	EngineVersion string
	HandoffDir    string
	EngineHost    string
	EnginePort    int
	// Overridable so a test or a sibling engine preset can point at its own.
	PGIsReady string
	PollWait  time.Duration
}

// Reads the cloud-init env file, then lets real env vars override.
func loadConfig(envFile string) config {
	get := guestenv.Load(envFile).Get

	cfg := config{
		GatewayURL:           get("RDS_GATEWAY_URL"),
		GatewayCA:            get("RDS_GATEWAY_CA"),
		Region:               get("RDS_REGION"),
		DBInstanceIdentifier: get("RDS_DB_INSTANCE_IDENTIFIER"),
		EngineVersion:        get("RDS_ENGINE_VERSION"),
		HandoffDir:           get("RDS_HANDOFF_DIR"),
		EngineHost:           get("RDS_ENGINE_HOST"),
		PGIsReady:            get("RDS_PG_ISREADY"),
		EnginePort:           defaultEnginePort,
		PollWait:             defaultPollWait,
	}
	if cfg.GatewayCA == "" {
		cfg.GatewayCA = defaultGatewayCA
	}
	if cfg.HandoffDir == "" {
		cfg.HandoffDir = defaultHandoffDir
	}
	if cfg.EngineHost == "" {
		cfg.EngineHost = defaultEngineHost
	}
	if cfg.PGIsReady == "" {
		cfg.PGIsReady = defaultPGIsReady
	}
	// The authoritative port comes from the bootstrap config; this is only what
	// the probe uses until that first fetch lands.
	if v := get("RDS_ENGINE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.EnginePort = p
		}
	}
	if v := get("RDS_POLL_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.PollWait = d
		}
	}
	return cfg
}
