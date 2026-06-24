package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Ensure env vars don't leak in from the host.
	for _, k := range []string{"ECS_GATEWAY_URL", "ECS_GATEWAY_CA", "ECS_REGION",
		"ECS_CLUSTER", "ECS_NATS_URL", "ECS_IMDS_BASE", "ECS_CONTAINERD_SOCKET"} {
		t.Setenv(k, "")
	}
	cfg := loadConfig(filepath.Join(t.TempDir(), "absent.env"))
	if cfg.GatewayCA != defaultGatewayCA {
		t.Errorf("GatewayCA = %q, want default", cfg.GatewayCA)
	}
	if cfg.IMDSBase != defaultIMDSBase {
		t.Errorf("IMDSBase = %q, want default", cfg.IMDSBase)
	}
	if cfg.ContainerdSocket != defaultContainerdSocket {
		t.Errorf("ContainerdSocket = %q, want default", cfg.ContainerdSocket)
	}
	if cfg.NATSURL != defaultNATSURL {
		t.Errorf("NATSURL = %q, want default", cfg.NATSURL)
	}
	if cfg.ClusterName != "default" {
		t.Errorf("ClusterName = %q, want default", cfg.ClusterName)
	}
	if cfg.Heartbeat != defaultHeartbeat {
		t.Errorf("Heartbeat = %v, want %v", cfg.Heartbeat, defaultHeartbeat)
	}
}

func TestLoadConfig_FileThenEnvOverride(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	body := "# comment\nECS_GATEWAY_URL=https://gw.file\nECS_REGION=us-west-2\nECS_CLUSTER=prod\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ECS_GATEWAY_URL", "https://gw.env")
	t.Setenv("ECS_NATS_URL", "nats://10.0.0.1:4222")

	cfg := loadConfig(envFile)
	if cfg.GatewayURL != "https://gw.env" {
		t.Errorf("GatewayURL = %q, want env override", cfg.GatewayURL)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("Region = %q, want from file", cfg.Region)
	}
	if cfg.ClusterName != "prod" {
		t.Errorf("ClusterName = %q, want from file", cfg.ClusterName)
	}
	if cfg.NATSURL != "nats://10.0.0.1:4222" {
		t.Errorf("NATSURL = %q, want env", cfg.NATSURL)
	}
}

func TestParseEnvFile_SkipsBlankAndComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.env")
	if err := os.WriteFile(p, []byte("\n# c\nA=1\nnokeyval\nB = 2 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parseEnvFile(p)
	if m["A"] != "1" || m["B"] != "2" {
		t.Errorf("parse mismatch: %#v", m)
	}
	if _, ok := m["nokeyval"]; ok {
		t.Errorf("line without = should be skipped")
	}
}
