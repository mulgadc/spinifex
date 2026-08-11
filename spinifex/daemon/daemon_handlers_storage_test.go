package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterTOML is the two-host topology both tests below read: host 1 runs a
// gate, a blob node and a meta replica; host 2 runs a second blob node.
const clusterTOML = `
version = 1
region = "ap-southeast-2"

[rs]
data = 2
parity = 1

[[host]]
id = 1
bind_addr = "0.0.0.0"
addr = "10.0.0.1"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host.node]]
id = 1
role = "gate"
port = 8443

[[host.node]]
id = 2
role = "blob"
port = 6660

[[host.node]]
id = 3
role = "meta"
port = 7660

[[host]]
id = 2
bind_addr = "0.0.0.0"
addr = "10.0.0.2"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host.node]]
id = 4
role = "blob"
port = 6660

[[bucket]]
name = "predastore"
region = "ap-southeast-2"
`

// storageConfigResponse installs cfg as the node's predastore.toml, drives the
// handler over NATS and returns what it answered.
func storageConfigResponse(t *testing.T, cfg string, reply string) []byte {
	t.Helper()

	dir := t.TempDir()
	if cfg != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "predastore"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "predastore", "predastore.toml"), []byte(cfg), 0o600))
	}

	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	replies := make(chan []byte, 1)
	sub, err := nc.Subscribe(reply, func(msg *nats.Msg) { replies <- msg.Data })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	d := &Daemon{configPath: filepath.Join(dir, "spinifex.toml")}
	d.handleStorageConfig(&nats.Msg{Subject: "storage.config", Reply: reply, Sub: sub})

	select {
	case payload := <-replies:
		return payload
	case <-time.After(5 * time.Second):
		t.Fatal("handleStorageConfig sent no response")
		return nil
	}
}

// TestHandleStorageConfig_SplitsNodesByRole pins what the gateway builds its
// storage report from: every node is reported at its host address and its own
// port, meta and blob in separate tables, and the gate in neither — it serves
// the S3 API and holds no cluster state to report.
func TestHandleStorageConfig_SplitsNodesByRole(t *testing.T) {
	payload := storageConfigResponse(t, clusterTOML, "test.storage.config.ok")

	var resp types.StorageConfigResponse
	require.NoError(t, json.Unmarshal(payload, &resp))

	assert.Equal(t, 2, resp.Encoding.DataShards)
	assert.Equal(t, 1, resp.Encoding.ParityShards)

	require.Len(t, resp.MetaNodes, 1)
	assert.Equal(t, types.StorageMetaNode{ID: 3, Host: "10.0.0.1", Port: 7660}, resp.MetaNodes[0])

	require.Len(t, resp.BlobNodes, 2)
	assert.Equal(t, types.StorageBlobNode{ID: 2, Host: "10.0.0.1", Port: 6660}, resp.BlobNodes[0])
	assert.Equal(t, types.StorageBlobNode{ID: 4, Host: "10.0.0.2", Port: 6660}, resp.BlobNodes[1])

	require.Len(t, resp.Buckets, 1)
	assert.Equal(t, "predastore", resp.Buckets[0].Name)
	assert.Equal(t, "ap-southeast-2", resp.Buckets[0].Region)
}

// TestHandleStorageConfig_ReportsAMissingConfig covers the node that has no
// predastore config: the report is an error rather than an empty topology,
// which would read as a cluster with no storage at all.
func TestHandleStorageConfig_ReportsAMissingConfig(t *testing.T) {
	payload := storageConfigResponse(t, "", "test.storage.config.absent")

	assert.Contains(t, string(payload), "InternalError")
}

// TestHandleStorageConfig_ReportsAMalformedConfig covers a config that parses
// as neither the current schema nor anything else: the same error, rather than
// a half-read topology.
func TestHandleStorageConfig_ReportsAMalformedConfig(t *testing.T) {
	payload := storageConfigResponse(t, "not = [toml", "test.storage.config.malformed")

	assert.Contains(t, string(payload), "InternalError")
}

// The storage status report is built by parsing predastore.toml directly, so
// it degrades silently when the schema moves under it: a config that no longer
// matches yields an empty topology rather than an error. This pins the parse
// to the [[host]]/[[host.node]] dialect the templates actually emit.
func TestPredastoreTOML_ParsesClusterTopology(t *testing.T) {
	var cfg predastoreTOML
	require.NoError(t, toml.Unmarshal([]byte(clusterTOML), &cfg))

	assert.Equal(t, 2, cfg.RS.Data)
	assert.Equal(t, 1, cfg.RS.Parity)
	require.Len(t, cfg.Hosts, 2)
	require.Len(t, cfg.Buckets, 1)

	assert.Equal(t, "10.0.0.2", cfg.Hosts[1].Addr)
	require.Len(t, cfg.Hosts[0].Nodes, 3)
	assert.Equal(t, predastoreRoleGate, cfg.Hosts[0].Nodes[0].Role)
	assert.Equal(t, 8443, cfg.Hosts[0].Nodes[0].Port)
	assert.Equal(t, predastoreRoleBlob, cfg.Hosts[0].Nodes[1].Role)
	assert.Equal(t, predastoreRoleMeta, cfg.Hosts[0].Nodes[2].Role)
	assert.Equal(t, 7660, cfg.Hosts[0].Nodes[2].Port)

	require.Len(t, cfg.Hosts[1].Nodes, 1)
	assert.Equal(t, 4, cfg.Hosts[1].Nodes[0].ID)
	assert.Equal(t, predastoreRoleBlob, cfg.Hosts[1].Nodes[0].Role)
	assert.Equal(t, 6660, cfg.Hosts[1].Nodes[0].Port)
	assert.Equal(t, "predastore", cfg.Buckets[0].Name)
}
