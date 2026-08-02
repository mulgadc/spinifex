package daemon

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
)

// predastoreTOML is a minimal representation of the predastore config file,
// containing only the fields needed for storage metrics (no credentials).
type predastoreTOML struct {
	RS      predastoreRS       `toml:"rs"`
	Hosts   []predastoreHost   `toml:"host"`
	Nodes   []predastoreNode   `toml:"node"`
	Buckets []predastoreBucket `toml:"buckets"`
}

type predastoreRS struct {
	Data   int `toml:"data"`
	Parity int `toml:"parity"`
}

// predastoreHost is one predastore process; the nodes pinned to it are all
// reachable at its public address.
type predastoreHost struct {
	ID         int    `toml:"id"`
	PublicAddr string `toml:"public_addr"`
}

// predastoreNode is a role pinned to a host. Its address is the host's, so
// reporting a node means resolving its host_id.
type predastoreNode struct {
	ID     int    `toml:"id"`
	HostID int    `toml:"host_id"`
	Role   string `toml:"role"`
}

// Predastore node roles, as written in the [[node]] blocks.
const (
	predastoreRoleShardStorage = "shard-storage"
	predastoreRoleStateReplica = "state-replica"
)

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

	// A node's address is its host's: every node pinned to a host is served
	// by that one process, keyed apart within it.
	hostAddrs := make(map[int]predastoreHost, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		hostAddrs[h.ID] = h
	}

	for _, n := range cfg.Nodes {
		host, port := splitPredastoreAddr(hostAddrs[n.HostID].PublicAddr)
		switch n.Role {
		case predastoreRoleStateReplica:
			resp.DBNodes = append(resp.DBNodes, types.StorageDBNode{ID: n.ID, Host: host, Port: port})
		case predastoreRoleShardStorage:
			resp.ShardNodes = append(resp.ShardNodes, types.StorageShardNode{ID: n.ID, Host: host, Port: port})
		default:
			slog.Warn("handleStorageConfig: unknown predastore node role", "node", n.ID, "role", n.Role)
		}
	}
	if resp.DBNodes == nil {
		resp.DBNodes = []types.StorageDBNode{}
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

// splitPredastoreAddr splits a host's "ip:port" into its parts. An address
// missing or malformed in the config yields the raw value and a zero port
// rather than dropping the node from the report entirely.
func splitPredastoreAddr(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}
