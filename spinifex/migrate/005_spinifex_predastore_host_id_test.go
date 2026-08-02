package migrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spinifexTomlFixture writes a spinifex.toml and returns its config directory.
func spinifexTomlFixture(t *testing.T, body string) string {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, spinifexRelPath), []byte(body), 0640))
	return configDir
}

func readSpinifexToml(t *testing.T, configDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, spinifexRelPath))
	require.NoError(t, err)
	return string(data)
}

// TestSpinifexPredastoreHostIDRename covers a multi-node member, where the key
// selects which predastore host this machine runs.
func TestSpinifexPredastoreHostIDRename(t *testing.T) {
	configDir := spinifexTomlFixture(t, `version = "3"

[nodes.node1]
node = "node1"
host = "10.0.0.1"

[nodes.node1.predastore]
host = "10.0.0.1:8443"
region = "ap-southeast-2"
bucket = "predastore"
node_id = 2

[nodes.node2]
node = "node2"
`)

	require.NoError(t, DefaultRegistry.RunConfig(spinifexTarget, configDir, t.TempDir()))

	got := readSpinifexToml(t, configDir)
	assert.Contains(t, got, "host_id = 2")
	assert.NotContains(t, got, "node_id")
	assert.Contains(t, got, `version = "4"`)

	// The surrounding tables are untouched.
	assert.Contains(t, got, `host = "10.0.0.1:8443"`)
	assert.Contains(t, got, "[nodes.node2]")
}

// TestSpinifexPredastoreHostIDLeavesOtherTables guards against renaming a
// node_id that belongs to something else — only the predastore section moved
// from naming a node to naming a host.
func TestSpinifexPredastoreHostIDLeavesOtherTables(t *testing.T) {
	configDir := spinifexTomlFixture(t, `version = "3"

[cluster]
node_id = 7

[nodes.node1.viperblock]
node_id = 9

[nodes.node1.predastore]
node_id = 2
`)

	require.NoError(t, DefaultRegistry.RunConfig(spinifexTarget, configDir, t.TempDir()))

	got := readSpinifexToml(t, configDir)
	assert.Contains(t, got, "[cluster]\nnode_id = 7")
	assert.Contains(t, got, "[nodes.node1.viperblock]\nnode_id = 9")
	assert.Contains(t, got, "[nodes.node1.predastore]\nhost_id = 2")
}

// TestSpinifexPredastoreHostIDSingleNode is the common install: the key is
// only emitted for multi-node, so there is nothing to rename.
func TestSpinifexPredastoreHostIDSingleNode(t *testing.T) {
	body := `version = "3"

[nodes.node1.predastore]
host = "0.0.0.0:8443"
bucket = "predastore"
`
	configDir := spinifexTomlFixture(t, body)

	require.NoError(t, DefaultRegistry.RunConfig(spinifexTarget, configDir, t.TempDir()))

	got := readSpinifexToml(t, configDir)
	assert.Contains(t, got, `version = "4"`)
	assert.Contains(t, got, `host = "0.0.0.0:8443"`)
	assert.NotContains(t, got, "host_id")
}
