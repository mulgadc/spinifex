package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetGlobalViper clears the global viper singleton derivePredastoreBind and
// config.LoadConfig share, so a fixture loaded by one test can't leak into
// another. Not run with t.Parallel() for the same reason.
func resetGlobalViper(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { viper.Reset() })
}

func writeSpinifexToml(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestDerivePredastoreBind_NodeIDConfigured(t *testing.T) {
	resetGlobalViper(t)

	path := writeSpinifexToml(t, `
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"
region = "us-east-1"
az = "us-east-1a"

[nodes.node1.predastore]
host = "0.0.0.0:8443"
bucket = "predastore"
region = "us-east-1"
node_id = 3
`)

	clusterConfig, err := config.LoadConfig(path)
	require.NoError(t, err)

	bind, err := derivePredastoreBind(clusterConfig)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", bind.Host)
	assert.Equal(t, 8443, bind.Port)
	assert.Equal(t, 3, bind.NodeID)
}

// TestDerivePredastoreBind_NodeIDAbsentDefaultsToColocated covers the
// co-located NODE_ID=-1 rule: single-node spinifex.toml templates omit
// node_id entirely, since every configured DB peer runs in this one process.
// predastore-start.sh defaulted to -1 for exactly that reason, and predastore
// itself rejects node_id=0, so an absent key must never silently resolve to
// the Go zero value instead.
func TestDerivePredastoreBind_NodeIDAbsentDefaultsToColocated(t *testing.T) {
	resetGlobalViper(t)

	path := writeSpinifexToml(t, `
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"
region = "us-east-1"
az = "us-east-1a"

[nodes.node1.predastore]
host = "0.0.0.0:8443"
bucket = "predastore"
region = "us-east-1"
`)

	clusterConfig, err := config.LoadConfig(path)
	require.NoError(t, err)

	bind, err := derivePredastoreBind(clusterConfig)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", bind.Host)
	assert.Equal(t, 8443, bind.Port)
	assert.Equal(t, -1, bind.NodeID)
}

// TestDerivePredastoreBind_BindHostSurvivesLoadConfigNormalization asserts
// derivePredastoreBind reads the raw configured bind host, not the
// DIAL-normalized one config.LoadConfig rewrites 0.0.0.0 -> 127.0.0.1 to for
// the local node's Predastore.Host field.
func TestDerivePredastoreBind_BindHostSurvivesLoadConfigNormalization(t *testing.T) {
	resetGlobalViper(t)

	path := writeSpinifexToml(t, `
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"

[nodes.node1.predastore]
host = "0.0.0.0:9443"
bucket = "predastore"
region = "us-east-1"
`)

	clusterConfig, err := config.LoadConfig(path)
	require.NoError(t, err)
	// Sanity check: confirm LoadConfig did normalize the struct field, so
	// this test actually exercises the bypass rather than a no-op.
	require.Equal(t, "127.0.0.1:9443", clusterConfig.Nodes["node1"].Predastore.Host)

	bind, err := derivePredastoreBind(clusterConfig)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", bind.Host, "must bind the raw configured host, not the DIAL-normalized loopback")
	assert.Equal(t, 9443, bind.Port)
}

func TestDerivePredastoreBind_MissingHostErrors(t *testing.T) {
	resetGlobalViper(t)

	clusterConfig := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {}},
	}

	_, err := derivePredastoreBind(clusterConfig)
	require.Error(t, err)
}

// TestPredastoreConfigPathNotShadowedByClusterConfigPathEnv is the
// regression test for the E2E bootstrap failure caused by viper's
// AutomaticEnv resolving before explicit BindEnv: SPINIFEX_CONFIG_PATH
// (the cluster config, bound to the unrelated "config" key) has the same
// AutomaticEnv-derived name as the bare "config-path" key used to have
// ("SPINIFEX_" + upper(key), "-"->"_"), so setting SPINIFEX_CONFIG_PATH
// alongside SPINIFEX_PREDASTORE_CONFIG_PATH made predastore load the
// cluster config as its own S3 config. This must resolve to the predastore
// path regardless of what else is set.
func TestPredastoreConfigPathNotShadowedByClusterConfigPathEnv(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
	viper.Reset()

	// Mirrors service.go's init(): AutomaticEnv setup plus the real
	// production binding under test, re-applied against the freshly-Reset
	// instance (pflags survive Reset since they live on predastoreCmd, a
	// package-level singleton already registered before any test runs).
	viper.SetEnvPrefix("SPINIFEX")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	bindPredastoreNamespacedEnv()

	t.Setenv("SPINIFEX_CONFIG_PATH", "/etc/spinifex/spinifex.toml")
	t.Setenv("SPINIFEX_PREDASTORE_CONFIG_PATH", "/etc/spinifex/predastore/predastore.toml")

	got := viper.GetString("predastore-config-path")
	assert.Equal(t, "/etc/spinifex/predastore/predastore.toml", got,
		"predastore-config-path must resolve to the predastore config even when SPINIFEX_CONFIG_PATH is also set")
}
