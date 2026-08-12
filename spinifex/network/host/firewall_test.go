package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	modePath   string

	// tableLoaded stands in for the loaded ruleset. Default true, so a test that
	// says nothing about it exercises the steady state rather than a reboot.
	tableLoaded atomic.Bool

	mu   sync.Mutex
	runs [][]string
}

func (e *firewallTestEnv) record(name string, args ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs = append(e.runs, append([]string{name}, args...))
}

// helperRuns copies under the lock: MaintainFirewall records from its own
// goroutine while the test reads.
func (e *firewallTestEnv) helperRuns() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.runs...)
}

func newFirewallTestEnv(t *testing.T, encap string) *firewallTestEnv {
	t.Helper()
	dir := t.TempDir()
	env := &firewallTestEnv{
		configPath: filepath.Join(dir, "spinifex.toml"),
		peersPath:  filepath.Join(dir, "peers.nft"),
		stdinPath:  filepath.Join(dir, "stdin"),
		modePath:   filepath.Join(dir, "mode"),
	}
	env.tableLoaded.Store(true)

	origPeers, origHelper, origEncap := firewallPeersPath, firewallApplyHelper, ovnEncapCommand
	origMode, origTable := firewallModePath, firewallTableCheck
	firewallPeersPath = env.peersPath
	firewallModePath = env.modePath
	firewallApplyHelper = "/usr/local/lib/spinifex/spinifex-firewall-apply"
	ovnEncapCommand = func(string) *exec.Cmd { return exec.Command("printf", "%s", encap) }
	firewallTableCheck = env.tableLoaded.Load
	t.Cleanup(func() {
		firewallPeersPath, firewallApplyHelper, ovnEncapCommand = origPeers, origHelper, origEncap
		firewallModePath, firewallTableCheck = origMode, origTable
	})

	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		env.record(name, args...)
		if len(args) > 0 && args[0] == "set-peers" {
			return exec.Command("sh", "-c", env.setPeersScript())
		}
		return exec.Command("true")
	}))

	return env
}

// setPeersScript stands in for the root helper: it keeps the payload for
// assertions and renders the same peer file. Rendering matters — code that skips
// an unchanged set reads that file back, so a stub that only captured stdin
// would make every reconcile look like a change.
func (e *firewallTestEnv) setPeersScript() string {
	return "cat > " + e.stdinPath + "\n" +
		"{ printf '%s\\n' '# Managed by spinifex-daemon. Regenerated from cluster membership.'\n" +
		"  printf 'define spinifex_peers = { %s }\\n' \"$(sed -n 1p " + e.stdinPath + " | sed 's/,/, /g')\"\n" +
		"  printf 'define spinifex_encap_peers = { %s }\\n' \"$(sed -n 2p " + e.stdinPath + " | sed 's/,/, /g')\"\n" +
		"} > " + e.peersPath + "\n"
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
	cfg.Network.FirewallEnabled = &enabled
	for name, node := range cfg.Nodes {
		node.VPCD.OVNSBAddr = "tcp:10.9.7.21:6642"
		cfg.Nodes[name] = node
	}
	return cfg
}

func TestReconcileFirewall(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))

	runs := env.helperRuns()
	require.Len(t, runs, 1)
	assert.Equal(t, []string{firewallApplyHelper, "set-peers"}, runs[0])

	// Both planes in the peer set, encap kept separate and narrower.
	lines := strings.Split(strings.TrimSpace(env.helperStdin(t)), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "10.9.7.21,10.9.7.22,10.9.8.21,10.9.8.22,192.168.1.21,192.168.1.22", lines[0])
	assert.Equal(t, "10.9.8.21,10.9.8.22", lines[1])
}

func TestReconcileFirewall_NoOpWhenPeersUnchanged(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")
	desired := renderPeersFile(
		[]string{"10.9.7.21", "10.9.7.22", "10.9.8.21", "10.9.8.22", "192.168.1.21", "192.168.1.22"},
		[]string{"10.9.8.21", "10.9.8.22"})
	require.NoError(t, os.WriteFile(env.peersPath, []byte(desired), 0644))

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))
	assert.Empty(t, env.helperRuns(), "an unchanged peer set must not re-apply the policy")
}

// The reboot case. A ruleset is runtime state, so the peer file survives and the
// table does not. Treating "peer set unchanged" as "policy applied" left the node
// open until something changed the membership, which on a healthy cluster is never.
func TestReconcileFirewall_ReappliesWhenTableMissing(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")
	desired := renderPeersFile(
		[]string{"10.9.7.21", "10.9.7.22", "10.9.8.21", "10.9.8.22", "192.168.1.21", "192.168.1.22"},
		[]string{"10.9.8.21", "10.9.8.22"})
	require.NoError(t, os.WriteFile(env.peersPath, []byte(desired), 0644))
	env.tableLoaded.Store(false)

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))
	require.Len(t, env.helperRuns(), 1, "an unchanged peer set with no loaded table must re-apply")

	// And having reapplied, it goes quiet again rather than re-applying on every tick.
	env.tableLoaded.Store(true)
	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))
	assert.Len(t, env.helperRuns(), 1)
}

// A node installed before the mode file existed has neither it nor the config
// key, and must keep the policy it already has.
func TestFirewallWanted_DefaultsOnWhenNothingSaysOtherwise(t *testing.T) {
	env := newFirewallTestEnv(t, "")
	_ = env

	cfg := &config.ClusterConfig{}
	assert.True(t, firewallWanted(cfg))
}

// The curl-to-bash install path writes "off" so a machine that was already
// running services does not get a default-deny policy uninvited.
func TestFirewallWanted_ModeFile(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"off", false},
		{"on", true},
		{"on\n", true},
		{" off \n", false},
	} {
		t.Run(strings.TrimSpace(tc.mode), func(t *testing.T) {
			env := newFirewallTestEnv(t, "")
			require.NoError(t, os.WriteFile(env.modePath, []byte(tc.mode), 0644))
			assert.Equal(t, tc.want, firewallWanted(&config.ClusterConfig{}))
		})
	}
}

// An operator's explicit choice outranks whatever the installer decided, in both
// directions.
func TestFirewallWanted_ConfigOverridesModeFile(t *testing.T) {
	env := newFirewallTestEnv(t, "")
	require.NoError(t, os.WriteFile(env.modePath, []byte("off"), 0644))
	assert.True(t, firewallWanted(firewallClusterConfig(true)))

	require.NoError(t, os.WriteFile(env.modePath, []byte("on"), 0644))
	assert.False(t, firewallWanted(firewallClusterConfig(false)))
}

// A brownfield install must not have a policy torn down or applied behind it: an
// "off" mode file with no peer file is the normal state there, and must be quiet.
func TestReconcileFirewall_ModeOffSkipsSilently(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")
	require.NoError(t, os.WriteFile(env.modePath, []byte("off"), 0644))

	cfg := firewallClusterConfig(true)
	cfg.Network.FirewallEnabled = nil
	require.NoError(t, ReconcileFirewall(env.configPath, cfg))
	assert.Empty(t, env.helperRuns(), "a disarmed node must not invoke the root helper")
}

// A missing Southbound answer must not narrow the set onto a guess: a peer file
// without every chassis's encap address drops Geneve between the nodes it omits.
func TestReconcileFirewall_EmptyEncapDoesNotWrite(t *testing.T) {
	env := newFirewallTestEnv(t, "")

	err := ReconcileFirewall(env.configPath, firewallClusterConfig(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0 of 2 chassis")
	assert.Empty(t, env.helperRuns())
}

// The case observed on a fresh 5-node bootstrap: one node's ovn-controller had
// not registered yet. Writing the partial answer would drop Geneve from every
// chassis missing from it.
func TestReconcileFirewall_PartialEncapDoesNotWrite(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")

	err := ReconcileFirewall(env.configPath, firewallClusterConfig(true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 of 2 chassis")
	assert.Empty(t, env.helperRuns(), "a partial chassis list must not reach the helper")
}

// A chassis left behind by a decommissioned node only widens the set, so it must
// not block the reconcile.
func TestReconcileFirewall_ExtraEncapStillWrites(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n10.9.8.23\n")

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(true)))
	require.Len(t, env.helperRuns(), 1)
}

// The reconcile must recover on its own. Before this, a node that lost the race
// with ovn-controller stayed unprotected until something restarted the daemon.
func TestMaintainFirewall_RetriesUntilChassisRegister(t *testing.T) {
	env := newFirewallTestEnv(t, "")

	var attempts atomic.Int32
	ovnEncapCommand = func(string) *exec.Cmd {
		// Two partial answers, then the full set — a bootstrap in miniature.
		switch attempts.Add(1) {
		case 1:
			return exec.Command("printf", "%s", "")
		case 2:
			return exec.Command("printf", "%s", "10.9.8.21\n")
		default:
			return exec.Command("printf", "%s", "10.9.8.21\n10.9.8.22\n")
		}
	}

	origRetry, origInterval := firewallRetryDelay, firewallReconcileInterval
	firewallRetryDelay = time.Millisecond
	firewallReconcileInterval = 50 * time.Millisecond
	t.Cleanup(func() { firewallRetryDelay, firewallReconcileInterval = origRetry, origInterval })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { MaintainFirewall(ctx, env.configPath, firewallClusterConfig(true)); close(done) }()

	require.Eventually(t, func() bool { return len(env.helperRuns()) > 0 }, 5*time.Second, 5*time.Millisecond,
		"loop must keep retrying until every chassis has registered")
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MaintainFirewall did not return when its context was cancelled")
	}
	assert.GreaterOrEqual(t, attempts.Load(), int32(3), "should have retried past the partial answers")
}

// An unchanged peer set must not re-apply the policy on every tick.
func TestMaintainFirewall_SteadyStateIsQuiet(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n10.9.8.22\n")

	origRetry, origInterval := firewallRetryDelay, firewallReconcileInterval
	firewallRetryDelay = time.Millisecond
	firewallReconcileInterval = time.Millisecond
	t.Cleanup(func() { firewallRetryDelay, firewallReconcileInterval = origRetry, origInterval })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	MaintainFirewall(ctx, env.configPath, firewallClusterConfig(true))

	// Many ticks, one apply: the rendered-file comparison short-circuits the rest.
	assert.Len(t, env.helperRuns(), 1)
}

func TestReconcileFirewall_DisabledRemovesThePolicy(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")
	require.NoError(t, os.WriteFile(env.peersPath, []byte("define x = { }\n"), 0644))

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(false)))

	runs := env.helperRuns()
	require.Len(t, runs, 1)
	assert.Equal(t, []string{firewallApplyHelper, "disable"}, runs[0])
}

// Disabled on a node that never had the policy is a no-op, not an error: this
// runs on every daemon start.
func TestReconcileFirewall_DisabledWithoutPolicyIsSilent(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")

	require.NoError(t, ReconcileFirewall(env.configPath, firewallClusterConfig(false)))
	assert.Empty(t, env.helperRuns())
}

func TestReconcileFirewall_NilConfigLeavesPolicyAlone(t *testing.T) {
	env := newFirewallTestEnv(t, "10.9.8.21\n")

	require.NoError(t, ReconcileFirewall(env.configPath, nil))
	assert.Empty(t, env.helperRuns())
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
