package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))

	if cfg.GatewayCA != defaultGatewayCA || cfg.HandoffDir != defaultHandoffDir {
		t.Errorf("CA/handoff = %q/%q, want the built-in defaults", cfg.GatewayCA, cfg.HandoffDir)
	}
	if cfg.EngineHost != defaultEngineHost || cfg.EnginePort != defaultEnginePort {
		t.Errorf("engine endpoint = %s:%d, want the local default", cfg.EngineHost, cfg.EnginePort)
	}
	if cfg.PGIsReady != defaultPGIsReady || cfg.PollWait != defaultPollWait {
		t.Errorf("probe/pollWait = %q/%v, want the built-in defaults", cfg.PGIsReady, cfg.PollWait)
	}
	// Identity is optional: the gateway resolves it from the caller's
	// credentials, so an agent with no configured identifier is normal.
	if cfg.DBInstanceIdentifier != "" {
		t.Errorf("DBInstanceIdentifier = %q, want empty when unconfigured", cfg.DBInstanceIdentifier)
	}
}

func TestLoadConfig_ReadsEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	body := "RDS_GATEWAY_URL=https://gw.internal:9999\n" +
		"RDS_REGION=ap-southeast-2\n" +
		"RDS_DB_INSTANCE_IDENTIFIER=db-1\n" +
		"RDS_ENGINE_VERSION=18.1\n" +
		"RDS_ENGINE_PORT=6543\n" +
		"RDS_POLL_WAIT=5s\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cfg := loadConfig(path)
	if cfg.GatewayURL != "https://gw.internal:9999" || cfg.Region != "ap-southeast-2" {
		t.Errorf("gateway/region = %q/%q, want the delivered values", cfg.GatewayURL, cfg.Region)
	}
	if cfg.DBInstanceIdentifier != "db-1" || cfg.EngineVersion != "18.1" {
		t.Errorf("identity = %q/%q, want db-1/18.1", cfg.DBInstanceIdentifier, cfg.EngineVersion)
	}
	if cfg.EnginePort != 6543 || cfg.PollWait != 5*time.Second {
		t.Errorf("port/pollWait = %d/%v, want 6543/5s", cfg.EnginePort, cfg.PollWait)
	}
}

// An unparseable override falls back to the default rather than leaving the
// agent with a zero port or a zero-length long poll.
func TestLoadConfig_IgnoresUnusableOverrides(t *testing.T) {
	t.Setenv("RDS_ENGINE_PORT", "not-a-port")
	t.Setenv("RDS_POLL_WAIT", "forever")

	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))
	if cfg.EnginePort != defaultEnginePort || cfg.PollWait != defaultPollWait {
		t.Errorf("port/pollWait = %d/%v, want the defaults", cfg.EnginePort, cfg.PollWait)
	}
}
