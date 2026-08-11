package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/guestenv"
)

const (
	defaultEnvFile    = "/etc/spinifex-rds/agent.env"
	defaultGatewayCA  = "/etc/spinifex-rds/gateway-ca.pem"
	defaultHandoffDir = "/run/spinifex-rds"
	defaultEngineHost = "127.0.0.1"
	// Where setup.sh stamps the engine the image bakes. The agent builds its
	// engine implementation from this file rather than from anything delivered.
	defaultEngineFile         = "/etc/spinifex-rds/engine"
	defaultRCService          = "rc-service"
	defaultEngineProbeTimeout = 5 * time.Second
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
	PollWait      time.Duration

	// The engine the control plane launched this VM as, delivered by cloud-init,
	// and the engine the image itself bakes. Two independent statements of the
	// same fact: the agent refuses to bootstrap when they disagree.
	Engine      string
	EngineFile  string
	BakedEngine string

	// Where the command handlers reach the engine: the client binaries, the
	// datadir the parameter file is installed into, the socket they connect
	// over, the OS user they drop to, and the service the stop goes through.
	EngineBinDir  string
	EngineDataDir string
	SocketDir     string
	EngineUser    string
	RCService     string
	EngineService string
	// Where the engine records the pid its probe checks for liveness, for an
	// engine whose own client cannot tell a server still recovering from one that
	// is not running at all.
	EnginePidFile      string
	EngineProbeTimeout time.Duration

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
		Engine:               get("RDS_ENGINE"),
		EngineFile:           get("RDS_ENGINE_FILE"),
		EngineVersion:        get("RDS_ENGINE_VERSION"),
		HandoffDir:           get("RDS_HANDOFF_DIR"),
		EngineHost:           get("RDS_ENGINE_HOST"),
		EngineBinDir:         get("RDS_ENGINE_BIN"),
		EngineDataDir:        get("RDS_ENGINE_DATA"),
		SocketDir:            get("RDS_SOCKET_DIR"),
		EngineUser:           get("RDS_ENGINE_USER"),
		RCService:            get("RDS_RC_SERVICE"),
		EngineService:        get("RDS_ENGINE_SERVICE"),
		EnginePidFile:        get("RDS_ENGINE_PIDFILE"),
		DataMount:            get("RDS_DATA_MOUNT"),
		PollWait:             defaultPollWait,
		EngineProbeTimeout:   defaultEngineProbeTimeout,
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
	if cfg.EngineFile == "" {
		cfg.EngineFile = defaultEngineFile
	}
	if cfg.RCService == "" {
		cfg.RCService = defaultRCService
	}
	// The image's own stamp rather than the delivered engine: an agent that took
	// its layout from the payload could run one engine's implementation against
	// another engine's datadir, which is the failure the stamp exists to prevent.
	cfg.BakedEngine = readBakedEngine(cfg.EngineFile)
	cfg.applyLayout(engineLayouts[cfg.BakedEngine])

	// rds-datadir reads the same RDS_DATA_MOUNT, so the two agree on where the
	// volume landed without either asserting it to the other.
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
	if v := get("RDS_ENGINE_PROBE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.EngineProbeTimeout = d
		}
	}
	return cfg
}

// Fills in whatever the delivered configuration left unset. An override always
// wins, so a test can point any of these at a fixture. An unrecognised engine
// leaves the layout empty, and New refuses rather than guessing at one.
func (c *config) applyLayout(layout engineLayout) {
	if c.EngineBinDir == "" {
		c.EngineBinDir = layout.binDir
	}
	if c.EngineDataDir == "" {
		c.EngineDataDir = layout.dataDir
	}
	if c.SocketDir == "" {
		c.SocketDir = layout.socketDir
	}
	if c.EngineUser == "" {
		c.EngineUser = layout.osUser
	}
	if c.EngineService == "" {
		c.EngineService = layout.service
	}
	if c.EnginePidFile == "" {
		c.EnginePidFile = layout.pidFile
	}
	if c.DataMount == "" {
		c.DataMount = layout.dataMount
	}
	if c.EnginePort == 0 {
		c.EnginePort = layout.port
	}
}

// The engine the image bakes. An absent or unreadable stamp reads as empty,
// which New refuses: an agent that guessed would defeat the stamp.
func readBakedEngine(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(raw)))
}
