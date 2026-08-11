package daemon

import (
	"testing"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The storage status report is built by parsing predastore.toml directly, so
// it degrades silently when the schema moves under it: a config that no longer
// matches yields an empty topology rather than an error. This pins the parse
// to the [[host]]/[[host.node]] dialect the templates actually emit.
func TestPredastoreTOML_ParsesClusterTopology(t *testing.T) {
	content := `
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

	var cfg predastoreTOML
	require.NoError(t, toml.Unmarshal([]byte(content), &cfg))

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
