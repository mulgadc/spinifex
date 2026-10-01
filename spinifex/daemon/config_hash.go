package daemon

import "github.com/mulgadc/spinifex/spinifex/bootstrap/config"

// sharedClusterData contains exactly the cluster-wide configuration inputs to
// computeConfigHash. Node-local top-level fields must remain excluded so every
// daemon derives the same hash from a shared cluster configuration.
type sharedClusterData struct {
	Epoch   uint64                   `json:"epoch" toml:"epoch"`
	Version string                   `json:"version" toml:"version"`
	Nodes   map[string]config.Config `json:"nodes" toml:"nodes"`
}
