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
	// defaultPollWait is the command long-poll window the agent asks the gateway
	// to hold a request open for. The gateway caps it at 20s.
	defaultPollWait = 20 * time.Second
)

// config is the static settings the agent reads at boot, delivered per-instance
// by cloud-init.
//
// It carries no secrets and no account identity by design: IMDS is readable by
// anything in the guest, so the master password reaches the VM only through an
// authenticated GetDBBootstrapConfig call. Nor does the agent state who it is —
// the gateway resolves the DB instance from the instance-role credentials the
// call is signed with. DBInstanceIdentifier is therefore optional; when set, it
// is sent so a mis-provisioned VM is rejected instead of acting on whatever
// instance the control plane maps it to.
type config struct {
	GatewayURL           string
	GatewayCA            string
	Region               string
	DBInstanceIdentifier string
	// EngineVersion is what this image actually ships. Empty leaves the control
	// plane's recorded version alone rather than clearing it.
	EngineVersion string
	HandoffDir    string
	EngineHost    string
	EnginePort    int
	// PGIsReady is the engine probe binary, overridable so a test or a sibling
	// engine preset can point at its own.
	PGIsReady string
	PollWait  time.Duration
}

// loadConfig reads the cloud-init env file then lets real env vars override.
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
