package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetViper(t *testing.T) {
	t.Cleanup(func() { viper.Reset() })
}

func TestLoadConfig_ValidTOMLFile(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"
region = "us-east-1"
az = "us-east-1a"
[nodes.node1.daemon]
host = "127.0.0.1:8080"

[nodes.node1.nats]
host = "127.0.0.1:4222"

[nodes.node1.nats.acl]
token = "nats_testtoken"

[nodes.node1.predastore]
host = "127.0.0.1:8443"
bucket = "predastore"
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, uint64(1), cfg.Epoch)
	assert.Equal(t, "node1", cfg.Node)
	assert.Equal(t, "1.0", cfg.Version)

	node, ok := cfg.Nodes["node1"]
	require.True(t, ok, "node1 should exist in Nodes map")
	assert.Equal(t, "us-east-1", node.Region)
	assert.Equal(t, "us-east-1a", node.AZ)
	assert.Equal(t, "127.0.0.1:8080", node.Daemon.Host)
	assert.Equal(t, "127.0.0.1:4222", node.NATS.Host)
	assert.Equal(t, "nats_testtoken", node.NATS.ACL.Token)
	assert.Equal(t, "127.0.0.1:8443", node.Predastore.Host)
	assert.Equal(t, "predastore", node.Predastore.Bucket)
}

func TestLoadConfig_MultipleNodes(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
epoch = 2
node = "leader"
version = "2.0"

[nodes.leader]
region = "us-east-1"

[nodes.follower]
region = "us-west-2"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Len(t, cfg.Nodes, 2)
	assert.Equal(t, "us-east-1", cfg.Nodes["leader"].Region)
	assert.Equal(t, "us-west-2", cfg.Nodes["follower"].Region)
}

func TestLoadConfig_EmptyConfigPath(t *testing.T) {
	resetViper(t)
	cfg, err := LoadConfig("")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	// All zero values
	assert.Equal(t, uint64(0), cfg.Epoch)
	assert.Empty(t, cfg.Node)
}

func TestLoadConfig_AWSDefaults(t *testing.T) {
	resetViper(t)
	cfg, err := LoadConfig("")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, DefaultAWSRegion, cfg.AWS.Region)
	assert.Equal(t, DefaultAWSInternalSuffix, cfg.AWS.InternalSuffix)
}

func TestLoadConfig_AWSOverride(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[aws]
region = "ap-southeast-2"
internal_suffix = "dev.local"

[nodes.n1]
region = "ap-southeast-2"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "ap-southeast-2", cfg.AWS.Region)
	assert.Equal(t, "dev.local", cfg.AWS.InternalSuffix)
}

func TestLoadConfig_NonexistentFile(t *testing.T) {
	resetViper(t)
	cfg, err := LoadConfig("/tmp/nonexistent-spinifex-config-test-12345.toml")
	// Not an error - falls through to defaults
	require.NoError(t, err)
	require.NotNil(t, cfg)
}

func TestLoadConfig_MalformedTOML(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("this is not valid toml {{{"), 0600))

	cfg, err := LoadConfig(path)
	assert.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "error reading config file")
}

func TestLoadConfig_PartialConfig(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.toml")
	require.NoError(t, os.WriteFile(path, []byte(`node = "partial-node"
epoch = 5
`), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "partial-node", cfg.Node)
	assert.Equal(t, uint64(5), cfg.Epoch)
	assert.Empty(t, cfg.Version)
	assert.Nil(t, cfg.Nodes)
}

func TestLoadConfig_EnvVarOverrideWithFile(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	// Viper's AutomaticEnv only works for keys Viper already knows about
	// (from a config file or explicit BindEnv). Provide a minimal config
	// so Viper registers the "epoch" key, then override via env.
	require.NoError(t, os.WriteFile(path, []byte(`epoch = 1
node = "file-node"
`), 0600))

	t.Setenv("SPINIFEX_NODE", "env-node")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	// Env vars override file values for keys Viper knows about
	assert.Equal(t, "env-node", cfg.Node)
}

func TestLoadConfig_NestedStructParsing(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "nested.toml")

	toml := `
node = "n1"

[nodes.n1]
region = "ap-southeast-2"

[nodes.n1.daemon]
host = "0.0.0.0:8080"
tlskey = "server.key"
tlscert = "server.pem"

[nodes.n1.nats]
host = "0.0.0.0:4222"

[nodes.n1.nats.acl]
token = "secret-token"

[nodes.n1.nats.sub]
subject = "test-subject"

[nodes.n1.predastore]
host = "0.0.0.0:8443"
bucket = "mybucket"
region = "ap-southeast-2"
accesskey = "AK"
secretkey = "SK"
base_dir = "/data"
node_id = 1

[nodes.n1.awsgw]
host = "0.0.0.0:9999"
debug = true
expected_nodes = 3
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	n := cfg.Nodes["n1"]
	assert.Equal(t, "0.0.0.0:8080", n.Daemon.Host)
	assert.Equal(t, "server.key", n.Daemon.TLSKey)
	assert.Equal(t, "server.pem", n.Daemon.TLSCert)
	assert.Equal(t, "0.0.0.0:4222", n.NATS.Host)
	assert.Equal(t, "secret-token", n.NATS.ACL.Token)
	assert.Equal(t, "test-subject", n.NATS.Sub.Subject)
	assert.Equal(t, "127.0.0.1:8443", n.Predastore.Host) // 0.0.0.0 normalized to loopback
	assert.Equal(t, "mybucket", n.Predastore.Bucket)
	assert.Equal(t, "AK", n.Predastore.AccessKey)
	assert.Equal(t, "/data", n.Predastore.BaseDir)
	assert.Equal(t, 1, n.Predastore.NodeID)
	assert.Equal(t, "0.0.0.0:9999", n.AWSGW.Host)
	assert.True(t, n.AWSGW.Debug)
	assert.Equal(t, 3, n.AWSGW.ExpectedNodes)
}

// Tests for HasService / GetServices

func TestHasService(t *testing.T) {
	tests := []struct {
		name     string
		services []string
		query    string
		want     bool
	}{
		{"explicit list, member", []string{"nats", "daemon"}, "nats", true},
		{"explicit list, second member", []string{"nats", "daemon"}, "daemon", true},
		{"explicit list, non-member", []string{"nats", "daemon"}, "predastore", false},
		{"explicit list, queried name unknown", []string{"nats"}, "unknown", false},
		// A typo in the configured list ("natz") must not satisfy a query for
		// the real service name — Contains is name-exact, no fuzzy match.
		{"explicit list with typo entry", []string{"natz"}, "nats", false},
		{"nil list defaults to AllServices", nil, "viperblock", true},
		{"empty list defaults to AllServices", []string{}, "ui", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Services: tt.services}
			assert.Equal(t, tt.want, c.HasService(tt.query))
		})
	}

	// Sanity: empty list grants every documented service.
	empty := Config{}
	for _, svc := range AllServices {
		assert.True(t, empty.HasService(svc), "empty list must include %q", svc)
	}
}

func TestGetServices(t *testing.T) {
	tests := []struct {
		name     string
		services []string
		want     []string
	}{
		{"nil defaults to AllServices", nil, AllServices},
		{"empty defaults to AllServices", []string{}, AllServices},
		{"explicit list returned as-is", []string{"nats", "predastore"}, []string{"nats", "predastore"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Services: tt.services}
			assert.Equal(t, tt.want, c.GetServices())
		})
	}
}

// Tests for NodeBaseDir

func TestNodeBaseDir_HappyPath(t *testing.T) {
	cc := &ClusterConfig{
		Node: "node1",
		Nodes: map[string]Config{
			"node1": {BaseDir: "/data/node1"},
		},
	}
	assert.Equal(t, "/data/node1", cc.NodeBaseDir())
}

func TestNodeBaseDir_NilConfig(t *testing.T) {
	var cc *ClusterConfig
	assert.Empty(t, cc.NodeBaseDir())
}

func TestNodeBaseDir_EmptyNode(t *testing.T) {
	cc := &ClusterConfig{
		Node: "",
		Nodes: map[string]Config{
			"node1": {BaseDir: "/data/node1"},
		},
	}
	assert.Empty(t, cc.NodeBaseDir())
}

func TestNodeBaseDir_NodeNotInMap(t *testing.T) {
	cc := &ClusterConfig{
		Node: "missing",
		Nodes: map[string]Config{
			"node1": {BaseDir: "/data/node1"},
		},
	}
	assert.Empty(t, cc.NodeBaseDir())
}

func TestNodeBaseDir_EmptyBaseDir(t *testing.T) {
	cc := &ClusterConfig{
		Node: "node1",
		Nodes: map[string]Config{
			"node1": {BaseDir: ""},
		},
	}
	assert.Empty(t, cc.NodeBaseDir())
}

// Tests for NetworkConfig (external pools)

func TestLoadConfig_NetworkExternalPools(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
range_start = "192.168.1.150"
range_end = "192.168.1.250"
gateway = "192.168.1.1"
prefix_len = 24

[[network.external_pools]]
name = "overflow"
range_start = "10.0.0.2"
range_end = "10.0.0.254"
gateway = "10.0.0.1"
prefix_len = 24
region = "us-east-1"
az = "us-east-1a"

[nodes.n1]
region = "us-east-1"

[nodes.n1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
external_interface = "enp0s3"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "pool", cfg.Network.ExternalMode)
	require.Len(t, cfg.Network.ExternalPools, 2)

	wan := cfg.Network.ExternalPools[0]
	assert.Equal(t, "wan", wan.Name)
	assert.Equal(t, "192.168.1.150", wan.RangeStart)
	assert.Equal(t, "192.168.1.250", wan.RangeEnd)
	assert.Equal(t, "192.168.1.1", wan.Gateway)
	assert.Equal(t, 24, wan.PrefixLen)
	assert.Empty(t, wan.Region)
	assert.Empty(t, wan.AZ)

	overflow := cfg.Network.ExternalPools[1]
	assert.Equal(t, "overflow", overflow.Name)
	assert.Equal(t, "us-east-1", overflow.Region)
	assert.Equal(t, "us-east-1a", overflow.AZ)

	n := cfg.Nodes["n1"]
	assert.Equal(t, "enp0s3", n.VPCD.ExternalInterface)
}

func TestLoadConfig_NetworkIPSecEnabledDefaultsTrue(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	// No [network] block at all — IPSec must default to true so AWS-parity
	// edge deployments encrypt intra-AZ Geneve without operator opt-in.
	toml := `
node = "n1"

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Network.IPSecEnabled, "default")
}

func TestLoadConfig_NetworkIPSecEnabledExplicitFalse(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	// Operator escape hatch for trusted single-rack lab deployments.
	toml := `
node = "n1"

[network]
ipsec_enabled = false

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Network.IPSecEnabled)
}

func TestLoadConfig_NetworkPoolDHCPSourceAccepted(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
source = "dhcp"
bind_bridge = "br-wan"
gateway = "192.168.1.1"
prefix_len = 24

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, cfg.Network.ExternalPools, 1)
	assert.Equal(t, "dhcp", cfg.Network.ExternalPools[0].Source)
	assert.Equal(t, "br-wan", cfg.Network.ExternalPools[0].BindBridge)
}

func TestLoadConfig_NetworkPoolDHCPSourceRequiresBindBridge(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
source = "dhcp"
gateway = "192.168.1.1"
prefix_len = 24

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "bind_bridge")
}

func TestLoadConfig_NetworkPoolDHCPRejectsRange(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
source = "dhcp"
bind_bridge = "br-wan"
range_start = "192.168.1.150"
range_end = "192.168.1.200"

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "range_start")
}

func TestLoadConfig_NetworkPoolUnknownSourceRejected(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
source = "magic"
gateway = "192.168.1.1"
prefix_len = 24

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "source=")
	assert.Contains(t, err.Error(), "magic")
}

func TestLoadConfig_ExternalDHCPRejected(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"
external_dhcp = true

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "external_dhcp")
}

func TestLoadConfig_DhcpBindBridgeRejected(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[nodes.n1.vpcd]
dhcp_bind_bridge = "br-wan"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "dhcp_bind_bridge")
}

func TestLoadConfig_PoolRangeOutsideCIDR(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
range_start = "10.99.0.10"
range_end = "10.99.0.50"
gateway = "192.168.1.1"
prefix_len = 24

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "not inside")
}

func TestLoadConfig_PoolsOverlap(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "a"
range_start = "192.168.1.10"
range_end = "192.168.1.50"
gateway = "192.168.1.1"
prefix_len = 24

[[network.external_pools]]
name = "b"
range_start = "192.168.1.40"
range_end = "192.168.1.80"
gateway = "192.168.1.1"
prefix_len = 24

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "overlaps")
}

func TestLoadConfig_NetworkDisabledByDefault(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	require.NoError(t, os.WriteFile(path, []byte(`node = "n1"`), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Empty(t, cfg.Network.ExternalMode)
	assert.Empty(t, cfg.Network.ExternalPools)
}

func TestLoadConfig_ExternalInterfacePerNode(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[nodes.n1.vpcd]
external_interface = "eth1"

[nodes.n2.vpcd]
external_interface = "eno2"

[nodes.n3.vpcd]
external_interface = "enp3s0"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "eth1", cfg.Nodes["n1"].VPCD.ExternalInterface)
	assert.Equal(t, "eno2", cfg.Nodes["n2"].VPCD.ExternalInterface)
	assert.Equal(t, "enp3s0", cfg.Nodes["n3"].VPCD.ExternalInterface)
}

// Tests for ViperblockConfig

func TestLoadConfig_ViperblockShardWAL_Explicit(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[nodes.n1]
region = "us-east-1"

[nodes.n1.viperblock]
shardwal = false
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	n := cfg.Nodes["n1"]
	require.NotNil(t, n.Viperblock.ShardWAL, "ShardWAL should be set when explicitly configured")
	assert.False(t, *n.Viperblock.ShardWAL)
}

func TestLoadConfig_ViperblockShardWAL_DefaultNil(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[nodes.n1]
region = "us-east-1"
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	n := cfg.Nodes["n1"]
	assert.Nil(t, n.Viperblock.ShardWAL, "ShardWAL should be nil when not configured (defaults to false in service)")
}

func TestLoadConfig_ViperblockShardWAL_True(t *testing.T) {
	resetViper(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")

	toml := `
node = "n1"

[nodes.n1]
region = "us-east-1"

[nodes.n1.viperblock]
shardwal = true
`
	require.NoError(t, os.WriteFile(path, []byte(toml), 0600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	n := cfg.Nodes["n1"]
	require.NotNil(t, n.Viperblock.ShardWAL)
	assert.True(t, *n.Viperblock.ShardWAL)
}

func TestParseEndpoints(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "tcp:127.0.0.1:6641", []string{"tcp:127.0.0.1:6641"}},
		{"cluster", "tcp:ip1:6641,tcp:ip2:6641,tcp:ip3:6641",
			[]string{"tcp:ip1:6641", "tcp:ip2:6641", "tcp:ip3:6641"}},
		{"whitespace and trailing comma", " tcp:ip1:6641 , tcp:ip2:6641 ,",
			[]string{"tcp:ip1:6641", "tcp:ip2:6641"}},
		{"only commas", ",,", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ParseEndpoints(tc.in))
		})
	}
}

func TestLoadConfig_NATExemptCIDRs(t *testing.T) {
	base := `
node = "n1"

[network]
external_mode = %q
nat_exempt_cidrs = [%s]

[nodes.n1]
region = "us-east-1"

[nodes.n1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
`
	write := func(t *testing.T, mode, cidrs string) string {
		t.Helper()
		resetViper(t)
		path := filepath.Join(t.TempDir(), "spinifex.toml")
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, base, mode, cidrs), 0600))
		return path
	}

	t.Run("valid in nat mode", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "nat", `"192.168.1.0/24", "172.16.0.0/12"`))
		require.NoError(t, err)
		assert.Equal(t, []string{"192.168.1.0/24", "172.16.0.0/12"}, cfg.Network.NATExemptCIDRs)
	})

	t.Run("rejected outside nat mode", func(t *testing.T) {
		_, err := LoadConfig(write(t, "pool", `"192.168.1.0/24"`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nat_exempt_cidrs")
	})

	t.Run("invalid CIDR rejected", func(t *testing.T) {
		_, err := LoadConfig(write(t, "nat", `"not-a-cidr"`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-cidr")
	})
}

func TestLoadConfig_DHCPMAC(t *testing.T) {
	base := `
node = "n1"

[network]
external_mode = "pool"

[[network.external_pools]]
name = "wan"
source = %q
%s
%s

[nodes.n1]
region = "us-east-1"

[nodes.n1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
`
	write := func(t *testing.T, source, bindBridge, dhcpMAC string) string {
		t.Helper()
		resetViper(t)
		path := filepath.Join(t.TempDir(), "spinifex.toml")
		bb := ""
		if bindBridge != "" {
			bb = fmt.Sprintf("bind_bridge = %q", bindBridge)
		}
		dm := ""
		if dhcpMAC != "" {
			dm = fmt.Sprintf("dhcp_mac = %q", dhcpMAC)
		}
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, base, source, bb, dm), 0600))
		return path
	}

	t.Run("interface on dhcp pool", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "dhcp", "wlan0", "interface"))
		require.NoError(t, err)
		assert.Equal(t, "interface", cfg.Network.ExternalPools[0].DHCPMAC)
	})

	t.Run("unknown value rejected", func(t *testing.T) {
		_, err := LoadConfig(write(t, "dhcp", "wlan0", "random"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dhcp_mac")
	})

	t.Run("rejected on static pool", func(t *testing.T) {
		_, err := LoadConfig(write(t, "static", "", "interface"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `dhcp_mac is only valid with source="dhcp"`)
	})
}
