package types

import "github.com/mulgadc/spinifex/spinifex/config"

// SharedClusterData contains only the shared cluster information (no node-specific top-level fields).
type SharedClusterData struct {
	Epoch   uint64                   `json:"epoch" toml:"epoch"`
	Version string                   `json:"version" toml:"version"`
	Nodes   map[string]config.Config `json:"nodes" toml:"nodes"`
}

// NodeHealthResponse is a daemon's reply to a node health check. Status is
// running or starting, Uptime is in seconds, and ConfigHash lets peers spot
// config drift.
type NodeHealthResponse struct {
	Node          string            `json:"node"`
	Status        string            `json:"status"`
	ConfigHash    string            `json:"config_hash"`
	Epoch         uint64            `json:"epoch"`
	Uptime        int64             `json:"uptime"`
	Services      []string          `json:"services"`
	ServiceHealth map[string]string `json:"service_health,omitempty"`
}
