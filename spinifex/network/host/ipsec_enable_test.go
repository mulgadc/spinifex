package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func multiNodeClusterConfig() *config.ClusterConfig {
	return &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {},
			"node2": {},
		},
	}
}

type recordingSudo struct {
	runs          [][]string
	activeOutput  string
	enabledOutput map[string]string
	activePerUnit map[string]string
}

func (r *recordingSudo) stub(name string, args ...string) *exec.Cmd {
	r.runs = append(r.runs, append([]string{name}, args...))
	if name != "systemctl" || len(args) < 2 {
		return exec.Command("true")
	}
	unit := args[len(args)-1]
	switch args[0] {
	case "is-active":
		if out, ok := r.activePerUnit[unit]; ok {
			return exec.Command("printf", "%s", out)
		}
		out := r.activeOutput
		if out == "" {
			out = "active\n"
		}
		return exec.Command("printf", "%s", out)
	case "is-enabled":
		if out, ok := r.enabledOutput[unit]; ok {
			return exec.Command("printf", "%s", out)
		}
		return exec.Command("printf", "%s", "enabled\n")
	}
	return exec.Command("true")
}

// helperRuns returns the IPsec state-helper invocations, dropping the
// is-enabled/is-active probes that precede them.
func (r *recordingSudo) helperRuns() [][]string {
	var out [][]string
	for _, run := range r.runs {
		if run[0] == ipsecStateHelper {
			out = append(out, run)
		}
	}
	return out
}

func multiNodeIPSecConfig(enabled bool) *config.ClusterConfig {
	cfg := multiNodeClusterConfig()
	cfg.Network.IPSecEnabled = enabled
	return cfg
}

func TestEnableOVNIPSec(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))

	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	// Worker path: no local NB socket → no ovn-nbctl call.
	origNBSock := ovnNBSocketPath
	ovnNBSocketPath = filepath.Join(configDir, "no-such-socket")
	t.Cleanup(func() { ovnNBSocketPath = origNBSock })

	require.NoError(t, EnableOVNIPSec(configPath, multiNodeClusterConfig()))

	require.Len(t, recorder.runs, 3)
	assert.Equal(t, []string{"systemctl", "is-active", "openvswitch-ipsec.service"}, recorder.runs[0])
	for _, run := range recorder.runs[1:] {
		assert.Equal(t, "ovs-vsctl", run[0])
		assert.Equal(t, "set", run[1])
		assert.Equal(t, "Open_vSwitch", run[2])
	}
	joined := strings.Join(recorder.runs[1], " ")
	assert.Contains(t, joined, "other_config:certificate="+filepath.Join(configDir, "ipsec", "peer.pem"))
	assert.Contains(t, joined, "other_config:private_key="+filepath.Join(configDir, "ipsec", "peer.key"))
	assert.Contains(t, joined, "other_config:ca_cert="+filepath.Join(configDir, "ca.pem"))
	assert.Contains(t, strings.Join(recorder.runs[2], " "), "other_config:ipsec_encapsulation=true")
}

func TestEnableOVNIPSec_Management(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	sockPath := filepath.Join(configDir, "ovnnb_db.sock")
	require.NoError(t, os.WriteFile(sockPath, []byte{}, 0600))
	origNBSock := ovnNBSocketPath
	ovnNBSocketPath = sockPath
	t.Cleanup(func() { ovnNBSocketPath = origNBSock })

	require.NoError(t, EnableOVNIPSec(configPath, multiNodeClusterConfig()))

	require.Len(t, recorder.runs, 4)
	assert.Equal(t, []string{"ovn-nbctl", "set", "NB_Global", ".", "ipsec=true"}, recorder.runs[3])
}

func TestEnableOVNIPSec_SingleNodeSkip(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run on single-node short-circuit; got %s %v", name, args)
		return exec.Command("true")
	}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))

	cfg := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {}},
	}
	require.NoError(t, EnableOVNIPSec(configPath, cfg))
}

func TestEnableOVNIPSec_MonitorIPSecInactive(t *testing.T) {
	recorder := &recordingSudo{activeOutput: "inactive\n"}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	origTimeout := systemctlActiveTimeout
	systemctlActiveTimeout = 100 * time.Millisecond
	t.Cleanup(func() { systemctlActiveTimeout = origTimeout })

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	err := EnableOVNIPSec(configPath, multiNodeClusterConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-monitor-ipsec")
	assert.Contains(t, err.Error(), "not active")

	// ovs-vsctl must NOT run — flip without live daemon is the silent-drop trap.
	for _, run := range recorder.runs {
		assert.NotEqual(t, "ovs-vsctl", run[0], "ovs-vsctl invoked despite dead daemon: %v", run)
	}
}

func TestEnableOVNIPSec_MissingCert(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run when cert files are absent")
		return exec.Command("true")
	}))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "ca.pem"), []byte("x"), 0600))

	err := EnableOVNIPSec(configPath, multiNodeClusterConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing IPsec credential")
}

func TestEnableOVNIPSec_NoConfigPath(t *testing.T) {
	err := EnableOVNIPSec("", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config path unset")
}

func TestReconcileOVNIPSec_DisabledStopsCharon(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false)))

	assert.Equal(t, [][]string{{ipsecStateHelper, "off"}}, recorder.helperRuns())

	for _, run := range recorder.runs {
		assert.NotEqual(t, "ovs-vsctl", run[0], "must not touch OVS when IPsec is off: %v", run)
	}
}

// The daemon has no systemctl grant, so a unit change must never be attempted
// directly: it would fail at the polkit prompt and leave charon listening.
func TestReconcileOVNIPSec_NeverCallsSystemctlDirectly(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false)))

	for _, run := range recorder.runs {
		if run[0] != "systemctl" {
			continue
		}
		assert.Contains(t, []string{"is-active", "is-enabled"}, run[1],
			"only read-only systemctl verbs may be run directly: %v", run)
	}
}

// A single-node cluster has no tunnels to protect, so charon must not be left
// listening even though ipsec_enabled defaults to true.
func TestReconcileOVNIPSec_SingleNodeStopsCharon(t *testing.T) {
	recorder := &recordingSudo{}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	cfg := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": {}}}
	cfg.Network.IPSecEnabled = true

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", cfg))

	assert.Equal(t, [][]string{{ipsecStateHelper, "off"}}, recorder.helperRuns())
}

func TestReconcileOVNIPSec_EnabledTurnsTheServicesOn(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{
			ovsIPSecUnit:          "disabled\n",
			strongswanStarterUnit: "disabled\n",
		},
		activePerUnit: map[string]string{
			ovsIPSecUnit:          "active\n",
			strongswanStarterUnit: "inactive\n",
		},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	origNBSock := ovnNBSocketPath
	ovnNBSocketPath = filepath.Join(configDir, "no-such-socket")
	t.Cleanup(func() { ovnNBSocketPath = origNBSock })

	require.NoError(t, ReconcileOVNIPSec(configPath, multiNodeIPSecConfig(true)))

	assert.Equal(t, [][]string{{ipsecStateHelper, "on"}}, recorder.helperRuns())

	// The enable path must still reach the OVS writes.
	var sawEncapsulation bool
	for _, run := range recorder.runs {
		if run[0] == "ovs-vsctl" && strings.Contains(strings.Join(run, " "), "ipsec_encapsulation=true") {
			sawEncapsulation = true
		}
	}
	assert.True(t, sawEncapsulation, "ipsec_encapsulation was never set: %v", recorder.runs)
}

func TestReconcileOVNIPSec_AlreadyInStateIsNoOp(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{strongswanStarterUnit: "disabled\n", ovsIPSecUnit: "disabled\n"},
		activePerUnit: map[string]string{strongswanStarterUnit: "inactive\n", ovsIPSecUnit: "inactive\n"},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false)))
	assert.Empty(t, recorder.helperRuns())
}

func TestReconcileOVNIPSec_UnknownUnitIsNotAnError(t *testing.T) {
	recorder := &recordingSudo{
		enabledOutput: map[string]string{
			strongswanStarterUnit: "Failed to get unit file state for strongswan-starter.service: No such file or directory\n",
			ovsIPSecUnit:          "Failed to get unit file state for openvswitch-ipsec.service: No such file or directory\n",
		},
		activePerUnit: map[string]string{
			strongswanStarterUnit: "inactive\n",
			ovsIPSecUnit:          "inactive\n",
		},
	}
	t.Cleanup(utils.SetSudoCommandForTest(recorder.stub))

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", multiNodeIPSecConfig(false)))
	assert.Empty(t, recorder.helperRuns())
}

// A nil cluster config means the intent is unknown; tearing down working
// tunnels on a guess would be worse than leaving them.
func TestReconcileOVNIPSec_NilConfigLeavesUnitsAlone(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		t.Fatalf("utils.SudoCommand must not run with a nil cluster config; got %s %v", name, args)
		return exec.Command("true")
	}))

	require.NoError(t, ReconcileOVNIPSec("/etc/spinifex/spinifex.toml", nil))
}
