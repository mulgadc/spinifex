package daemon

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
)

// predastoreTOML is a minimal representation of the predastore config file,
// containing only the fields needed for storage metrics (no credentials).
type predastoreTOML struct {
	RS      predastoreRS       `toml:"rs"`
	DB      []predastoreDBNode `toml:"db"`
	Nodes   []predastoreNode   `toml:"nodes"`
	Buckets []predastoreBucket `toml:"buckets"`
}

type predastoreRS struct {
	Data   int `toml:"data"`
	Parity int `toml:"parity"`
}

type predastoreDBNode struct {
	ID   int    `toml:"id"`
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

type predastoreNode struct {
	ID   int    `toml:"id"`
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

type predastoreBucket struct {
	Name   string `toml:"name"`
	Type   string `toml:"type"`
	Region string `toml:"region"`
}

// handleStorageConfig responds with parsed predastore config (topology only, no creds).
// Used by the gateway to build the GetStorageStatus response.
func (d *Daemon) handleStorageConfig(msg *nats.Msg) {
	configDir := filepath.Dir(d.configPath)
	predastorePath := filepath.Join(configDir, "predastore", "predastore.toml")

	data, err := os.ReadFile(predastorePath)
	if err != nil {
		slog.Debug("handleStorageConfig: failed to read predastore config", "path", predastorePath, "err", err)
		respondWithError(msg, "InternalError")
		return
	}

	var cfg predastoreTOML
	if err := toml.Unmarshal(data, &cfg); err != nil {
		slog.Error("handleStorageConfig: failed to parse predastore config", "err", err)
		respondWithError(msg, "InternalError")
		return
	}

	resp := types.StorageConfigResponse{
		Encoding: types.StorageEncoding{
			DataShards:   cfg.RS.Data,
			ParityShards: cfg.RS.Parity,
		},
	}

	for _, db := range cfg.DB {
		resp.DBNodes = append(resp.DBNodes, types.StorageDBNode{
			ID:   db.ID,
			Host: db.Host,
			Port: db.Port,
		})
	}
	if resp.DBNodes == nil {
		resp.DBNodes = []types.StorageDBNode{}
	}

	for _, n := range cfg.Nodes {
		resp.ShardNodes = append(resp.ShardNodes, types.StorageShardNode{
			ID:   n.ID,
			Host: n.Host,
			Port: n.Port,
		})
	}
	if resp.ShardNodes == nil {
		resp.ShardNodes = []types.StorageShardNode{}
	}

	for _, b := range cfg.Buckets {
		resp.Buckets = append(resp.Buckets, types.StorageBucket{
			Name:   b.Name,
			Type:   b.Type,
			Region: b.Region,
		})
	}
	if resp.Buckets == nil {
		resp.Buckets = []types.StorageBucket{}
	}

	respondWithJSON(msg, resp)
}
