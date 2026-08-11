package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stamps the engine a real image bakes and loads the configuration against it,
// which is what selects the guest layout the agent's defaults come from.
func testLoadConfig(t *testing.T, engine string) config {
	t.Helper()
	stamp := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(stamp, []byte(engine+"\n"), 0o444); err != nil {
		t.Fatalf("write the engine stamp: %v", err)
	}
	t.Setenv("RDS_ENGINE_FILE", stamp)
	return loadConfig(filepath.Join(t.TempDir(), "absent.env"))
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg := testLoadConfig(t, enginePostgres)

	if cfg.GatewayCA != defaultGatewayCA || cfg.HandoffDir != defaultHandoffDir {
		t.Errorf("CA/handoff = %q/%q, want the built-in defaults", cfg.GatewayCA, cfg.HandoffDir)
	}
	if cfg.EngineHost != defaultEngineHost || cfg.PollWait != defaultPollWait {
		t.Errorf("engine host/pollWait = %q/%v, want the built-in defaults", cfg.EngineHost, cfg.PollWait)
	}
	// Identity is optional: the gateway resolves it from the caller's
	// credentials, so an agent with no configured identifier is normal.
	if cfg.DBInstanceIdentifier != "" {
		t.Errorf("DBInstanceIdentifier = %q, want empty when unconfigured", cfg.DBInstanceIdentifier)
	}
}

// The guest layout is the image's to state, not the control plane's: everything
// the agent reaches the engine through comes from the stamp it was built with.
func TestLoadConfig_TakesTheLayoutFromTheBakedEngine(t *testing.T) {
	cfg := testLoadConfig(t, enginePostgres)

	if cfg.BakedEngine != enginePostgres {
		t.Fatalf("baked engine = %q, want %q", cfg.BakedEngine, enginePostgres)
	}
	layout := engineLayouts[enginePostgres]
	if cfg.EngineBinDir != layout.binDir || cfg.EngineDataDir != layout.dataDir {
		t.Errorf("bin/data = %q/%q, want the PostgreSQL layout", cfg.EngineBinDir, cfg.EngineDataDir)
	}
	if cfg.EngineUser != layout.osUser || cfg.EngineService != layout.service {
		t.Errorf("user/service = %q/%q, want the PostgreSQL layout", cfg.EngineUser, cfg.EngineService)
	}
	if cfg.SocketDir != layout.socketDir || cfg.DataMount != layout.dataMount {
		t.Errorf("socket/mount = %q/%q, want the PostgreSQL layout", cfg.SocketDir, cfg.DataMount)
	}
	if cfg.EnginePort != layout.port {
		t.Errorf("port = %d, want the PostgreSQL default %d", cfg.EnginePort, layout.port)
	}
}

// An image with no stamp cannot say which engine it is, and an agent that
// guessed would be the failure the stamp exists to prevent.
func TestNewEngine_RefusesAnImageWithNoEngineStamp(t *testing.T) {
	t.Setenv("RDS_ENGINE_FILE", filepath.Join(t.TempDir(), "absent-engine"))
	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))

	if _, err := newProbe(cfg, staticProbe(0)); err == nil {
		t.Error("newProbe built a probe for an image carrying no engine stamp")
	}
	if _, err := newEngine(cfg, nil, nil, nil); err == nil {
		t.Error("newEngine built an engine for an image carrying no engine stamp")
	}
}

func TestNewEngine_RefusesAnEngineItDoesNotImplement(t *testing.T) {
	cfg := testLoadConfig(t, "mariadb")

	_, err := newEngine(cfg, nil, nil, nil)
	if err == nil {
		t.Fatal("newEngine built an engine this agent does not implement")
	}
	if !strings.Contains(err.Error(), "mariadb") {
		t.Errorf("error = %v, want it to name the engine the image bakes", err)
	}
}

func TestLoadConfig_ReadsEnvFile(t *testing.T) {
	t.Setenv("RDS_ENGINE_FILE", filepath.Join(t.TempDir(), "absent-engine"))
	path := filepath.Join(t.TempDir(), "agent.env")
	body := "RDS_GATEWAY_URL=https://gw.internal:9999\n" +
		"RDS_REGION=ap-southeast-2\n" +
		"RDS_DB_INSTANCE_IDENTIFIER=db-1\n" +
		"RDS_ENGINE=postgres\n" +
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
	if cfg.Engine != enginePostgres {
		t.Errorf("delivered engine = %q, want the one cloud-init carried", cfg.Engine)
	}
	if cfg.EnginePort != 6543 || cfg.PollWait != 5*time.Second {
		t.Errorf("port/pollWait = %d/%v, want 6543/5s", cfg.EnginePort, cfg.PollWait)
	}
}

// An unparseable override falls back to the layout's own default rather than
// leaving the agent with a zero port or a zero-length long poll.
func TestLoadConfig_IgnoresUnusableOverrides(t *testing.T) {
	t.Setenv("RDS_ENGINE_PORT", "not-a-port")
	t.Setenv("RDS_POLL_WAIT", "forever")

	cfg := testLoadConfig(t, enginePostgres)
	if cfg.EnginePort != engineLayouts[enginePostgres].port || cfg.PollWait != defaultPollWait {
		t.Errorf("port/pollWait = %d/%v, want the defaults", cfg.EnginePort, cfg.PollWait)
	}
}
