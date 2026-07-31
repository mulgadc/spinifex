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

	// The guest layout rds-init lays down. Overridable for the same reason it is
	// there: a sibling engine preset points at its own paths.
	defaultPGBin         = "/usr/libexec/postgresql18"
	defaultPGData        = "/var/lib/postgresql/18/data"
	defaultSocketDir     = "/run/postgresql"
	defaultPGUser        = "postgres"
	defaultRCService     = "rc-service"
	defaultEngineSvcName = "postgresql"
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

	// Where the command handlers reach the engine: the client binaries, the
	// datadir the parameter file is installed into, the socket they connect
	// over, the OS user they drop to, and the service the stop goes through.
	PGBin         string
	PGData        string
	SocketDir     string
	PGUser        string
	RCService     string
	EngineService string

	// Where the data volume is mounted, and the two kernel surfaces a storage
	// grow resolves its device from. Overridable so a test can point them at
	// fixtures rather than at the running host's.
	DataMount  string
	MountsFile string
	SysBlock   string
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
		PGBin:                get("RDS_PG_BIN"),
		PGData:               get("RDS_PGDATA"),
		SocketDir:            get("RDS_SOCKET_DIR"),
		PGUser:               get("RDS_PG_USER"),
		RCService:            get("RDS_RC_SERVICE"),
		EngineService:        get("RDS_ENGINE_SERVICE"),
		DataMount:            get("RDS_DATA_MOUNT"),
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
	if cfg.PGBin == "" {
		cfg.PGBin = defaultPGBin
	}
	if cfg.PGData == "" {
		cfg.PGData = defaultPGData
	}
	if cfg.SocketDir == "" {
		cfg.SocketDir = defaultSocketDir
	}
	if cfg.PGUser == "" {
		cfg.PGUser = defaultPGUser
	}
	if cfg.RCService == "" {
		cfg.RCService = defaultRCService
	}
	if cfg.EngineService == "" {
		cfg.EngineService = defaultEngineSvcName
	}
	// rds-datadir reads the same RDS_DATA_MOUNT, so the two agree on where the
	// volume landed without either asserting it to the other.
	if cfg.DataMount == "" {
		cfg.DataMount = defaultDataMount
	}
	cfg.MountsFile = defaultMountsFile
	cfg.SysBlock = defaultSysBlock
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
