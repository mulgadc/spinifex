package main

import (
	"bufio"
	"os"
	"strings"
	"time"
)

const (
	defaultEnvFile          = "/etc/spinifex-ecs/agent.env"
	defaultGatewayCA        = "/etc/spinifex-ecs/gateway-ca.pem"
	defaultIMDSBase         = "http://169.254.169.254/latest"
	defaultContainerdSocket = "/run/containerd/containerd.sock"
	defaultNATSURL          = "nats://127.0.0.1:4222"
	defaultHeartbeat        = 30 * time.Second
)

// config holds the static settings the agent reads at boot. Cluster identity
// (account ID, instance ID) is discovered at runtime from IMDS, not configured;
// only the cluster *name* the instance was launched into is static.
type config struct {
	GatewayURL       string
	GatewayCA        string
	Region           string
	ClusterName      string
	NATSURL          string
	IMDSBase         string
	ContainerdSocket string
	Heartbeat        time.Duration
}

// loadConfig reads the cloud-init env file then lets real env vars override.
func loadConfig(envFile string) config {
	env := parseEnvFile(envFile)
	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return env[key]
	}

	cfg := config{
		GatewayURL:       get("ECS_GATEWAY_URL"),
		GatewayCA:        get("ECS_GATEWAY_CA"),
		Region:           get("ECS_REGION"),
		ClusterName:      get("ECS_CLUSTER"),
		NATSURL:          get("ECS_NATS_URL"),
		IMDSBase:         get("ECS_IMDS_BASE"),
		ContainerdSocket: get("ECS_CONTAINERD_SOCKET"),
	}
	if cfg.GatewayCA == "" {
		cfg.GatewayCA = defaultGatewayCA
	}
	if cfg.IMDSBase == "" {
		cfg.IMDSBase = defaultIMDSBase
	}
	if cfg.ContainerdSocket == "" {
		cfg.ContainerdSocket = defaultContainerdSocket
	}
	if cfg.NATSURL == "" {
		cfg.NATSURL = defaultNATSURL
	}
	if cfg.ClusterName == "" {
		cfg.ClusterName = "default"
	}
	cfg.Heartbeat = defaultHeartbeat
	return cfg
}

// parseEnvFile reads a simple KEY=value file; missing files yield an empty map.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}
