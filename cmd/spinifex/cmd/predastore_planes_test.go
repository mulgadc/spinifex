package cmd

import (
	"os"
	"path/filepath"
	"testing"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadRenderedPredastore parses a rendered template with predastore's own
// loader. Asserting on the TOML text would only prove the template renders;
// this proves the cluster it describes is one predastore will start.
func loadRenderedPredastore(t *testing.T, content string) *pds.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "predastore.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	cfg, err := pds.LoadConfig(path)
	require.NoError(t, err, "the installer must not write a config predastore rejects")
	return cfg
}

// singleNodeSettings is what a real install renders with: the paths predastore
// validates are absolute, which the shared fixture leaves empty because the
// tests around it only read the rendered text.
func singleNodeSettings() admin.ConfigSettings {
	settings := northstarSettings()
	settings.ConfigDir = "/etc/spinifex"
	settings.PredastoreDataDir = admin.PredastoreDataDir("/var/lib/spinifex")
	return settings
}

func gateNode(t *testing.T, host pds.HostConfig) pds.NodeConfig {
	t.Helper()
	for _, n := range host.Nodes {
		if n.Role == pds.RoleGate {
			return n
		}
	}
	t.Fatalf("host %d runs no gate", host.ID)
	return pds.NodeConfig{}
}

// A single machine holds every shard of an object whatever the erasure code
// is, so parity there costs a split, an encode and a second write for a copy
// the same disk failure takes with it. Redundancy belongs to the pool under
// the data directory.
func TestPredastoreSingleNodeTemplateUsesNoParity(t *testing.T) {
	t.Parallel()

	cfg := loadRenderedPredastore(t, renderTemplate(t, predastoreTomlTemplate, singleNodeSettings()))

	assert.Equal(t, 1, cfg.RS.Data, "a single node splits nothing")
	assert.Equal(t, 0, cfg.RS.Parity, "a single node has nowhere to put parity")
}

// A single node is one server: one of each role. A second blob node would only
// hash-partition the keys across another index on the same drive, and a second
// meta node would add an fsync to every commit for a quorum sharing the disk it
// exists to survive.
func TestPredastoreSingleNodeTemplateRunsOneOfEachRole(t *testing.T) {
	t.Parallel()

	cfg := loadRenderedPredastore(t, renderTemplate(t, predastoreTomlTemplate, singleNodeSettings()))

	require.Len(t, cfg.Hosts, 1, "a single node is one host")

	byRole := make(map[pds.Role]int)
	for _, n := range cfg.Hosts[0].Nodes {
		byRole[n.Role]++
	}
	assert.Equal(t, map[pds.Role]int{pds.RoleGate: 1, pds.RoleBlob: 1, pds.RoleMeta: 1}, byRole)
}

// S3 is a public service and replication is not. The gate binds every
// interface; raft and blob traffic bind the host's own address and stay there.
func TestPredastoreSingleNodeTemplateSplitsThePlanes(t *testing.T) {
	t.Parallel()

	cfg := loadRenderedPredastore(t, renderTemplate(t, predastoreTomlTemplate, singleNodeSettings()))

	require.Len(t, cfg.Hosts, 1)
	assert.Equal(t, "0.0.0.0", gateNode(t, cfg.Hosts[0]).BindAddr, "S3 must answer on every interface")
	assert.NotEqual(t, "0.0.0.0", cfg.Hosts[0].BindAddr, "the cluster plane must not be a wildcard")
}

func TestPredastoreMultiNodeTemplateSplitsThePlanes(t *testing.T) {
	t.Parallel()

	content, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, predastoreMultinodeNodes(), "AK", "SK", "ap-southeast-2", "tok",
		"/cfg", "/var/lib/spinifex", "10.0.0.1", 0, admin.NorthstarCredentials{},
	)
	require.NoError(t, err)

	cfg := loadRenderedPredastore(t, content)

	require.Len(t, cfg.Hosts, 3)
	for _, host := range cfg.Hosts {
		// Every machine's replication plane is the address its peers dial, so
		// a second interface on any of them carries no cluster traffic.
		assert.Equal(t, host.Addr, host.BindAddr, "host %d binds the cluster plane off its dial address", host.ID)
		assert.Equal(t, "0.0.0.0", gateNode(t, host).BindAddr, "host %d must serve public S3", host.ID)
	}
}

// The bind_addr under [[host.node]] is the gate's alone: no other role has a
// listener to point anywhere, and predastore rejects one that tries.
func TestPredastoreTemplatesBindOnlyTheGate(t *testing.T) {
	t.Parallel()

	multinode, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, predastoreMultinodeNodes(), "AK", "SK", "ap-southeast-2", "tok",
		"/cfg", "/var/lib/spinifex", "10.0.0.1", 0, admin.NorthstarCredentials{},
	)
	require.NoError(t, err)

	for name, content := range map[string]string{
		"single-node": renderTemplate(t, predastoreTomlTemplate, singleNodeSettings()),
		"multi-node":  multinode,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, host := range loadRenderedPredastore(t, content).Hosts {
				for _, n := range host.Nodes {
					if n.Role != pds.RoleGate {
						assert.Empty(t, n.BindAddr, "node %d has role %q and must bind nothing of its own", n.ID, n.Role)
					}
				}
			}
		})
	}
}
