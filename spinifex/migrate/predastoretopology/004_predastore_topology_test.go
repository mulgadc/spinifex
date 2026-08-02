package predastoretopology

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/predastore/s3"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the predastore migration golden files")

// predastoreFixture lays out a v4 install under a temporary root: the config
// in place, and a data directory for each node this machine owns.
func predastoreFixture(t *testing.T, name string, nodeDirs []string) (configDir, dataDir string) {
	t.Helper()

	root := t.TempDir()
	configDir = filepath.Join(root, "etc")
	dataDir = filepath.Join(root, "var")

	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "predastore"), 0750))

	src, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, predastoreRelPath), src, 0640))

	for _, dir := range nodeDirs {
		full := filepath.Join(dataDir, "predastore", dir)
		require.NoError(t, os.MkdirAll(full, 0750))
		// A marker per directory proves the contents travelled, not just the path.
		require.NoError(t, os.WriteFile(filepath.Join(full, "marker"), []byte(dir), 0640))
	}
	return configDir, dataDir
}

// runPredastoreMigration drives the real registry path, so registration, the
// backup and the version stamp are covered alongside the rewrite itself.
func runPredastoreMigration(t *testing.T, configDir, dataDir string) {
	t.Helper()
	require.NoError(t, migrate.DefaultRegistry.RunConfig(predastoreTarget, configDir, dataDir))
}

// dataDirPlaceholder stands in for the test's temporary root, which is
// rendered into data_dir and cannot be checked in.
const dataDirPlaceholder = "{{DATA_DIR}}"

// assertGolden compares the migrated config against its golden file, which is
// checked in so a review sees the exact config an operator ends up with.
func assertGolden(t *testing.T, configDir, dataDir, golden string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(configDir, predastoreRelPath))
	require.NoError(t, err)
	got := strings.ReplaceAll(string(raw), dataDir, dataDirPlaceholder)

	path := filepath.Join("testdata", golden)
	if *updateGolden {
		require.NoError(t, os.WriteFile(path, []byte(got), 0640))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

// readMigratedConfig parses the migrated config the way predastore does, so
// the test fails on anything predastore would reject rather than on text.
func readMigratedConfig(t *testing.T, configDir, dataDir string) *s3.Config {
	t.Helper()

	cfg := &s3.Config{
		ConfigPath: filepath.Join(configDir, predastoreRelPath),
		BasePath:   filepath.Join(dataDir, "predastore"),
	}
	require.NoError(t, cfg.ReadConfig())
	return cfg
}

// TestPredastoreTopologySingleNode covers the common install: three db peers
// and three shard nodes on one machine collapse to one host running all six.
func TestPredastoreTopologySingleNode(t *testing.T) {
	configDir, dataDir := predastoreFixture(t, "predastore_v4_singlenode.toml", []string{
		"distributed/db/node-1", "distributed/db/node-2", "distributed/db/node-3",
		"distributed/nodes/node-1", "distributed/nodes/node-2", "distributed/nodes/node-3",
	})

	runPredastoreMigration(t, configDir, dataDir)
	assertGolden(t, configDir, dataDir, "predastore_v5_singlenode.golden")

	cfg := readMigratedConfig(t, configDir, dataDir)

	require.Len(t, cfg.Hosts, 1)
	assert.Equal(t, 1, cfg.Hosts[0].ID)
	assert.Equal(t, "127.0.0.1:6660", cfg.Hosts[0].BindAddr)
	assert.Equal(t, "127.0.0.1:6660", cfg.Hosts[0].PublicAddr)
	assert.Equal(t, filepath.Join(dataDir, "predastore", "cluster"), cfg.Hosts[0].DataDir)

	require.Len(t, cfg.ClusterNodes, 6)
	for i, n := range cfg.ClusterNodes {
		assert.Equal(t, i+1, n.ID)
		assert.Equal(t, 1, n.HostID)
		if i < 3 {
			assert.Equal(t, "shard-storage", string(n.Role))
		} else {
			assert.Equal(t, "state-replica", string(n.Role))
		}
	}

	// Shard nodes keep their ids because object metadata records which node
	// holds each shard; the state replicas are the ones that move.
	cluster := filepath.Join(dataDir, "predastore", "cluster")
	assertNodeDirContents(t, cluster, map[int]string{
		1: "distributed/nodes/node-1",
		2: "distributed/nodes/node-2",
		3: "distributed/nodes/node-3",
		4: "distributed/db/node-1",
		5: "distributed/db/node-2",
		6: "distributed/db/node-3",
	})

	assert.NoDirExists(t, filepath.Join(dataDir, "predastore", "distributed", "db", "node-1"))
	assert.NoDirExists(t, filepath.Join(dataDir, "predastore", "distributed", "nodes", "node-1"))

	// Everything outside the topology is the operator's, and must survive.
	raw, err := os.ReadFile(filepath.Join(configDir, predastoreRelPath))
	require.NoError(t, err)
	for _, keep := range []string{
		`[ratelimit.action."s3:PutObject"]`,
		"access_keys_bucket = \"spinifex-iam-access-keys\"",
		"interval_seconds = 300",
		`{ bucket = "predastore", actions = ["s3:ListBucket",  "s3:GetObject",`,
	} {
		assert.Contains(t, string(raw), keep)
	}
	assert.NotContains(t, string(raw), "\nhost = \"0.0.0.0\"")
	assert.NotContains(t, string(raw), "[[db]]")
	assert.NotContains(t, string(raw), "[[nodes]]")
}

// TestPredastoreTopologyMultiNode covers a three-machine cluster from the
// perspective of one member, which only holds its own node directories.
func TestPredastoreTopologyMultiNode(t *testing.T) {
	configDir, dataDir := predastoreFixture(t, "predastore_v4_multinode.toml", []string{
		"distributed/db/node-2", "distributed/nodes/node-2",
	})

	runPredastoreMigration(t, configDir, dataDir)
	assertGolden(t, configDir, dataDir, "predastore_v5_multinode.golden")

	cfg := readMigratedConfig(t, configDir, dataDir)

	require.Len(t, cfg.Hosts, 3)
	for i, h := range cfg.Hosts {
		assert.Equal(t, i+1, h.ID)
		// Binding every interface preserves the v4 template's reachability;
		// only the advertised address is per machine.
		assert.Equal(t, "0.0.0.0:6660", h.BindAddr)
		assert.Equal(t, fmt.Sprintf("10.0.0.%d:6660", h.ID), h.PublicAddr)
		assert.Equal(t, filepath.Join(dataDir, "predastore", "cluster"), h.DataDir)
	}

	require.Len(t, cfg.ClusterNodes, 6)
	byID := map[int]struct {
		host int
		role string
	}{}
	for _, n := range cfg.ClusterNodes {
		byID[n.ID] = struct {
			host int
			role string
		}{n.HostID, string(n.Role)}
	}
	for id, want := range map[int]struct {
		host int
		role string
	}{
		1: {1, "shard-storage"}, 2: {2, "shard-storage"}, 3: {3, "shard-storage"},
		4: {1, "state-replica"}, 5: {2, "state-replica"}, 6: {3, "state-replica"},
	} {
		assert.Equal(t, want, byID[id], "node %d", id)
	}

	// Only this machine's directories exist; the rest belong to its peers.
	cluster := filepath.Join(dataDir, "predastore", "cluster")
	assertNodeDirContents(t, cluster, map[int]string{
		2: "distributed/nodes/node-2",
		5: "distributed/db/node-2",
	})
	assert.NoDirExists(t, filepath.Join(cluster, "node-1"))
	assert.NoDirExists(t, filepath.Join(cluster, "node-4"))
}

// TestPredastoreTopologyRerunIsNoop covers a run that failed after writing the
// config but before the version was stamped: the migration is invoked directly,
// so the version gate cannot mask a missing guard on the already-migrated file.
func TestPredastoreTopologyRerunIsNoop(t *testing.T) {
	configDir, dataDir := predastoreFixture(t, "predastore_v4_singlenode.toml", []string{
		"distributed/db/node-1", "distributed/nodes/node-1",
	})
	ctx := migrate.ConfigContext{ConfigDir: configDir, DataDir: dataDir, Logger: slog.Default()}

	require.NoError(t, migratePredastoreTopology(ctx))
	first, err := os.ReadFile(filepath.Join(configDir, predastoreRelPath))
	require.NoError(t, err)

	require.NoError(t, migratePredastoreTopology(ctx))
	second, err := os.ReadFile(filepath.Join(configDir, predastoreRelPath))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assertNodeDirContents(t, filepath.Join(dataDir, "predastore", "cluster"), map[int]string{
		1: "distributed/nodes/node-1",
		4: "distributed/db/node-1",
	})
}

// TestPredastoreTopologyRejectsNonV4Config fails loudly rather than writing a
// config with no topology at all.
func TestPredastoreTopologyRejectsNonV4Config(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "etc")
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "predastore"), 0750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, predastoreRelPath),
		[]byte("version = \"4\"\nregion = \"ap-southeast-2\"\n"), 0640))

	err := migratePredastoreTopology(migrate.ConfigContext{
		ConfigDir: configDir,
		DataDir:   t.TempDir(),
		Logger:    slog.Default(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a v4 distributed topology")
}

// TestPredastoreVersionReaderReadsFixtures ties the migration's FromVersion to
// what the reader actually finds in a shipped config.
func TestPredastoreVersionReaderReadsFixtures(t *testing.T) {
	r := &migrate.TOMLVersionReader{}
	for _, name := range []string{"predastore_v4_singlenode.toml", "predastore_v4_multinode.toml"} {
		v, err := r.ReadVersion(filepath.Join("testdata", name))
		require.NoError(t, err)
		assert.Equal(t, 4, v, name)
	}
}

// assertNodeDirContents checks that each node directory holds the marker
// written into the v4 directory it came from.
func assertNodeDirContents(t *testing.T, clusterDir string, want map[int]string) {
	t.Helper()
	for id, origin := range want {
		marker := filepath.Join(clusterDir, fmt.Sprintf("node-%d", id), "marker")
		got, err := os.ReadFile(marker)
		if !assert.NoError(t, err, "node-%d", id) {
			continue
		}
		assert.Equal(t, origin, string(got), "node-%d", id)
	}
}
