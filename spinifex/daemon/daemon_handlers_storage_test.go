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
// to the [[host]]/[[node]] dialect the templates actually emit.
func TestPredastoreTOML_ParsesClusterTopology(t *testing.T) {
	content := `
[rs]
data = 2
parity = 1

[[host]]
id = 1
bind_addr = "0.0.0.0:6660"
public_addr = "10.0.0.1:6660"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host]]
id = 2
bind_addr = "0.0.0.0:6660"
public_addr = "10.0.0.2:6660"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[node]]
id = 1
host_id = 1
role = "shard-storage"

[[node]]
id = 2
host_id = 2
role = "shard-storage"

[[node]]
id = 3
host_id = 1
role = "state-replica"

[[buckets]]
name = "predastore"
type = "distributed"
region = "ap-southeast-2"
`

	var cfg predastoreTOML
	require.NoError(t, toml.Unmarshal([]byte(content), &cfg))

	assert.Equal(t, 2, cfg.RS.Data)
	assert.Equal(t, 1, cfg.RS.Parity)
	require.Len(t, cfg.Hosts, 2)
	require.Len(t, cfg.Nodes, 3)

	assert.Equal(t, "10.0.0.2:6660", cfg.Hosts[1].PublicAddr)
	assert.Equal(t, predastoreRoleShardStorage, cfg.Nodes[0].Role)
	assert.Equal(t, 1, cfg.Nodes[2].HostID)
	assert.Equal(t, predastoreRoleStateReplica, cfg.Nodes[2].Role)
}

func TestSplitPredastoreAddr(t *testing.T) {
	host, port := splitPredastoreAddr("10.0.0.1:6660")
	assert.Equal(t, "10.0.0.1", host)
	assert.Equal(t, 6660, port)

	// A hand-edited address without a port must still name its node rather
	// than dropping it from the report.
	host, port = splitPredastoreAddr("10.0.0.1")
	assert.Equal(t, "10.0.0.1", host)
	assert.Equal(t, 0, port)

	host, port = splitPredastoreAddr("")
	assert.Empty(t, host)
	assert.Equal(t, 0, port)
}
