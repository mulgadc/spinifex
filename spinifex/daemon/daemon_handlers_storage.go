package daemon

import (
	"log/slog"
	"os"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
)

// predastoreTOML is a minimal representation of the predastore config file,
// containing only the fields needed for storage metrics (no credentials).
type predastoreTOML struct {
	RS      predastoreRS       `toml:"rs"`
	Hosts   []predastoreHost   `toml:"host"`
	Buckets []predastoreBucket `toml:"bucket"`
}

type predastoreRS struct {
	Data   int `toml:"data"`
	Parity int `toml:"parity"`
}

// predastoreHost is one predastore process, as written under [[host]]. Addr
// names the machine and carries no port; the nodes pinned to it supply their
// own.
type predastoreHost struct {
	ID    int              `toml:"id"`
	Addr  string           `toml:"addr"`
	Nodes []predastoreNode `toml:"node"`
}

// predastoreNode is a role pinned to the host it is written under. Its address
// is that host's addr joined with this port.
type predastoreNode struct {
	ID   int    `toml:"id"`
	Role string `toml:"role"`
	Port int    `toml:"port"`
}

// Predastore node roles, as written in the [[host.node]] blocks.
const (
	predastoreRoleGate = "gate"
	predastoreRoleBlob = "blob"
	predastoreRoleMeta = "meta"
)

type predastoreBucket struct {
	Name   string `toml:"name"`
	Region string `toml:"region"`
}

// handleStorageConfig responds with parsed predastore config (topology only, no creds).
// Used by the gateway to build the GetStorageStatus response.
func (d *Daemon) handleStorageConfig(msg *nats.Msg) string {
	predastorePath := predastoreConfigPath(d.configPath)

	data, err := os.ReadFile(predastorePath)
	if err != nil {
		slog.Debug("handleStorageConfig: failed to read predastore config", "path", predastorePath, "err", err)
		respondWithError(d.node, msg, "InternalError")
		return outcomeError
	}

	var cfg predastoreTOML
	if err := toml.Unmarshal(data, &cfg); err != nil {
		slog.Error("handleStorageConfig: failed to parse predastore config", "err", err)
		respondWithError(d.node, msg, "InternalError")
		return outcomeError
	}

	resp := types.StorageConfigResponse{
		Encoding: types.StorageEncoding{
			DataShards:   cfg.RS.Data,
			ParityShards: cfg.RS.Parity,
		},
	}

	// A node's address is its host's addr plus its own port: colocated nodes
	// share a machine but never a port.
	for _, h := range cfg.Hosts {
		for _, n := range h.Nodes {
			switch n.Role {
			case predastoreRoleMeta:
				resp.MetaNodes = append(resp.MetaNodes, types.StorageMetaNode{ID: n.ID, Host: h.Addr, Port: n.Port})
			case predastoreRoleBlob:
				resp.BlobNodes = append(resp.BlobNodes, types.StorageBlobNode{ID: n.ID, Host: h.Addr, Port: n.Port})
			case predastoreRoleGate:
				// A gate serves the S3 API and holds no cluster state, so it
				// belongs to neither table this report carries.
			default:
				slog.Warn("handleStorageConfig: unknown predastore node role", "node", n.ID, "role", n.Role)
			}
		}
	}
	if resp.MetaNodes == nil {
		resp.MetaNodes = []types.StorageMetaNode{}
	}
	if resp.BlobNodes == nil {
		resp.BlobNodes = []types.StorageBlobNode{}
	}

	for _, b := range cfg.Buckets {
		resp.Buckets = append(resp.Buckets, types.StorageBucket{
			Name:   b.Name,
			Region: b.Region,
		})
	}
	if resp.Buckets == nil {
		resp.Buckets = []types.StorageBucket{}
	}

	respondWithJSON(d.node, msg, resp)
	return outcomeSuccess
}
