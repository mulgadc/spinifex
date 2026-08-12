package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firewallTestEnv points the package vars at a temp dir and stubs both the
// Southbound query and the root helper. stdinPath receives whatever the helper
// was fed, which is the payload the peer sets are built from.
type firewallTestEnv struct {
	configPath string
	peersPath  string
	stdinPath  string
	runs       [][]string
}

func newFirewallTestEnv(t *testing.T, encap string) *firewallTestEnv {
	t.Helper()
	dir := t.TempDir()
	env := &firewallTestEnv{
		configPath: filepath.Join(dir, "spinifex.toml"),
		peersPath:  filepath.Join(dir, "peers.nft"),
		stdinPath:  filepath.Join(dir, "stdin"),
	}

	origPeers, origHelper, origEncap := firewallPeersPath, firewallApplyHelper, ovnEncapCommand
	firewallPeersPath = env.peersPath
	firewallApplyHelper = "/usr/local/lib/spinifex/spinifex-firewall-apply"
	ovnEncapCommand = func(string) *exec.Cmd { return exec.Command("printf", "%s", encap) }
	t.Cleanup(func() {
		firewallPeersPath, firewallApplyHelper, ovnEncapCommand = origPeers, origHelper, origEncap
	})

	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		env.runs = append(env.runs, append([]string{name}, args...))
		return exec.Command("sh", "-c", "cat > "+env.stdinPath)
	}))

	return env
}

func (e *firewallTestEnv) helperStdin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(e.stdinPath)
	require.NoError(t, err)
	return string(data)
}

func firewallClusterConfig(enabled bool) *config.ClusterConfig {
	cfg := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {Host: "10.9.7.21", AdvertiseIP: "192.168.1.21"},
			"node2": {Host: "10.9.7.22", AdvertiseIP: "192.168.1.22"},
		},
	}
	cfg.Network.FirewallEnabled = enabled
	for name, node := range cfg.Nodes {
		node.VPCD.OVNSBAddr = "tcp:10.9.7.21:6642"
		cfg.Nodes[name] = node
	}
	return cfg
}

func TestReconcileFirewall(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))

	require.Len(t, env.runs, 1)
	assert.Equal(t, []string{firewallApplyHelper, "set-peers"}, env.runs[0])

	// Both planes in the peer set, encap kept separate and narrower.
	lines := strings.Split(strings.TrimSpace(env.helperStdin(t)), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "10.9.7.21,10.9.7.22,10.9.8.21,10.9.8.22,192.168.1.21,192.168.1.22", lines[0])
	assert.Equal(t, "10.9.8.21,10.9.8.22", lines[1])
}

func TestReconcileFirewall_NoOpWhenPeersUnchanged(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")
	desired := renderPeersFile(
		[]string{"10.9.7.21", "10.9.7.22", "10.9.8.21", "192.168.1.21", "192.168.1.22"},
		[]string{"10.9.8.21"})
	require.NoError(t, os.WriteFile(env.peersPath, []byte(desired), 0644))

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))
	assert.Empty(t, env.runs, "an unchanged peer set must not re-apply the policy")
}

// A missing Southbound answer must not narrow the set onto a guess: a peer file
// without every chassis's encap address drops Geneve between the nodes it omits.
func TestReconcileFirewall_EmptyEncapDoesNotWrite(t *testing.T) {
	env := newFirewallTestEnv(t, "")

	err := ReconcileFirewall(env.configPath, firewallClusterConfig(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no encap addresses")
	assert.Empty(t, env.runs)
}

func TestReconcileFirewall_DisabledRemovesThePolicy(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")
	require.NoError(t, os.WriteFile(env.peersPath, []byte("define x = { }\n"), 0644))

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(false)))

	require.Len(t, env.runs, 1)
	assert.Equal(t, []string{firewallApplyHelper, "disable"}, env.runs[0])
}

// Disabled on a node that never had the policy is a no-op, not an error: this
// runs on every daemon start.
func TestReconcileFirewall_DisabledWithoutPolicyIsSilent(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(false)))
	assert.Empty(t, env.runs)
}

func TestReconcileFirewall_NilConfigLeavesPolicyAlone(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")

	require.NoError(t, ReconcileFirewall(env.configPath, nil))
	assert.Empty(t, env.runs)
}

func TestClusterPeerAddrsRejectsNonIPv4(t *testing.T) {
	cfg := &config.ClusterConfig{
		Nodes: map[string]config.Config{
			"a": {Host: "10.9.7.21", AdvertiseIP: "node1.example.com"},
			"b": {Host: "10.9.7.22:4432", AdvertiseIP: "0.0.0.0"},
			"c": {Host: "", AdvertiseIP: "::1"},
		},
	}
	assert.Equal(t, []string{"10.9.7.21"}, clusterPeerAddrs("", cfg, nil))
}

// The lan plane is the case a node's own config cannot supply: peers appear
// there by their wan address only, so a set built from config alone drops the
// cluster traffic they actually send.
func TestClusterPeerAddrsUnionsAllThreePlanes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nats"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nats", "nats.conf"), []byte(`
cluster {
  listen: 10.9.7.5:4248
  routes = [
    "nats-route://tok_secret@10.9.7.2:4248",
    "nats-route://tok_secret@10.9.7.4:4248",
  ]
}
`), 0644))

	cfg := &config.ClusterConfig{
		Nodes: map[string]config.Config{
			"local": {Host: "10.9.7.5", AdvertiseIP: "192.168.1.25"},
			"peer":  {Host: "192.168.1.21"},
		},
	}
	local := cfg.Nodes["local"]
	local.VPCD.OVNSBAddr = "tcp:10.9.7.2:6642,tcp:10.9.7.3:6642"
	local.VPCD.OVNNBAddr = "tcp:10.9.7.2:6641,tcp:10.9.7.3:6641"
	cfg.Nodes["local"] = local

	assert.Equal(t, []string{
		"10.9.7.2", "10.9.7.3", "10.9.7.4", "10.9.7.5", "10.9.8.1",
		"192.168.1.21", "192.168.1.25",
	}, clusterPeerAddrs(configPath, cfg, []string{"10.9.8.1"}))
}

// The route token must not be mistaken for the host.
func TestNATSRouteAddrsIgnoresTheToken(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nats"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nats", "nats.conf"),
		[]byte(`routes = [ "nats-route://nats_1.2.3.4tok@10.9.7.2:4248" ]`), 0644))

	assert.Equal(t, []string{"10.9.7.2"}, natsRouteAddrs(filepath.Join(dir, "spinifex.toml")))
	assert.Nil(t, natsRouteAddrs(""))
	assert.Nil(t, natsRouteAddrs(filepath.Join(t.TempDir(), "spinifex.toml")))
}

func TestIsPlainIPv4(t *testing.T) {
	for _, addr := range []string{"10.9.7.21", "192.168.1.1", "255.255.255.255"} {
		assert.True(t, isPlainIPv4(addr), addr)
	}
	for _, addr := range []string{"", "0.0.0.0", "10.9.7", "10.9.7.21:4432",
		"10.9.7.1234", "10.9.7.a", "::1", "10.9.7.21/24", "10.0.0.999", "::ffff:10.0.0.1", "127.0.0.1"} {
		assert.False(t, isPlainIPv4(addr), addr)
	}
}
