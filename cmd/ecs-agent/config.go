package main

import (
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/internal/guestenv"
)

const (
	defaultEnvFile          = "/etc/spinifex-ecs/agent.env"
	defaultGatewayCA        = "/etc/spinifex-ecs/gateway-ca.pem"
	defaultIMDSBase         = "http://169.254.169.254/latest"
	defaultContainerdSocket = "/run/containerd/containerd.sock"
	defaultHeartbeat        = 30 * time.Second
	defaultPollInterval     = 5 * time.Second
)

// config holds the static settings the agent reads at boot. Cluster identity
// (account ID, instance ID) is discovered at runtime from IMDS, not configured;
// only the cluster *name* the instance was launched into is static. AccessKey /
// SecretKey are legacy seeded creds, optional now that the agent SigV4-signs
// gateway calls with instance-role credentials from IMDS; the agent never holds
// a NATS token.
type config struct {
	GatewayURL       string
	GatewayCA        string
	Region           string
	ClusterName      string
	AccessKey        string
	SecretKey        string
	IMDSBase         string
	ContainerdSocket string
	Heartbeat        time.Duration
	PollInterval     time.Duration
	CredEndpointIP   string
	CredEndpointPort int
}

// loadConfig reads the cloud-init env file then lets real env vars override.
func loadConfig(envFile string) config {
	get := guestenv.Load(envFile).Get

	cfg := config{
		GatewayURL:       get("ECS_GATEWAY_URL"),
		GatewayCA:        get("ECS_GATEWAY_CA"),
		Region:           get("ECS_REGION"),
		ClusterName:      get("ECS_CLUSTER"),
		AccessKey:        get("ECS_ACCESS_KEY"),
		SecretKey:        get("ECS_SECRET_KEY"),
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
	if cfg.ClusterName == "" {
		cfg.ClusterName = "default"
	}
	cfg.Heartbeat = defaultHeartbeat
	if v := get("ECS_HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Heartbeat = d
		}
	}
	cfg.PollInterval = defaultPollInterval
	if v := get("ECS_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.PollInterval = d
		}
	}
	cfg.CredEndpointIP = get("ECS_CRED_ENDPOINT_IP")
	if cfg.CredEndpointIP == "" {
		cfg.CredEndpointIP = defaultCredEndpointIP
	}
	cfg.CredEndpointPort = defaultCredEndpointPort
	if v := get("ECS_CRED_ENDPOINT_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.CredEndpointPort = p
		}
	}
	return cfg
}
