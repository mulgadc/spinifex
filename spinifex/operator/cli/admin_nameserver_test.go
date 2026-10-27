package cli

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/runtime/formation"
)

// renderFormed renders node1's spinifex.toml for a formed cluster and loads it
// back the way every service does.
func renderFormed(t *testing.T, nodes map[string]formation.NodeInfo) *config.ClusterConfig {
	t.Helper()
	resetGlobalViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node: "node1", Az: "us-east-1a", Region: "us-east-1", Port: "4432", BindIP: "10.0.0.1",
		AccessKey: "AKIATEST", SecretKey: "SECRET", AccountID: "123456789012", NatsToken: "token", ConfigDir: dir,
		NameserverNodes: formation.NameserverNodes(nodes),
		RemoteNodes:     buildRemoteNodes(nodes, "node1", "/etc/spinifex/northstar.toml"),
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))
	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	return cfg
}

func formedNodes(nameservers ...string) map[string]formation.NodeInfo {
	nodes := map[string]formation.NodeInfo{}
	for i, name := range []string{"node1", "node2", "node3"} {
		nodes[name] = formation.NodeInfo{Name: name, BindIP: "10.0.0." + string(rune('1'+i)), Region: "us-east-1", AZ: "us-east-1a"}
	}
	for _, name := range nameservers {
		n := nodes[name]
		n.Nameserver = true
		nodes[name] = n
	}
	return nodes
}

// Nodes that named northstar in --services are pinned in [northstar], in name
// order, so every node of the cluster renders and loads the same pins.
func TestFormedConfigPinsTheNodesThatAskedToBeNameservers(t *testing.T) {
	cfg := renderFormed(t, formedNodes("node3", "node2"))
	assert.Equal(t, []string{"node2", "node3"}, cfg.Northstar.NameserverNodes)
}

// With no node asking, the block is left out and the first four northstar
// nodes by name are published.
func TestFormedConfigWithoutPinsLeavesNameserversAutomatic(t *testing.T) {
	cfg := renderFormed(t, formedNodes())
	assert.Empty(t, cfg.Northstar.NameserverNodes)
	assert.Zero(t, cfg.Northstar.NameserverCount)
}

func TestSingleNodeNameservers(t *testing.T) {
	assert.Nil(t, singleNodeNameservers("node1", false))
	assert.Equal(t, []string{"node1"}, singleNodeNameservers("node1", true))
}
