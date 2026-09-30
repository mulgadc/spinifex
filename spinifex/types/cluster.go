package types

import "github.com/mulgadc/spinifex/spinifex/config"

// SharedClusterData contains only the shared cluster information (no node-specific top-level fields).
type SharedClusterData struct {
	Epoch   uint64                   `json:"epoch" toml:"epoch"`
	Version string                   `json:"version" toml:"version"`
	Nodes   map[string]config.Config `json:"nodes" toml:"nodes"`
}
