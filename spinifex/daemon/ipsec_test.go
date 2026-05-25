package daemon

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

// multiNodeClusterConfig returns a ClusterConfig with two nodes so
// enableOVNIPSec runs its full path (single-node would short-circuit).
func multiNodeClusterConfig() *config.ClusterConfig {
	return &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {},
			"node2": {},
		},
	}
}

// recordingSudo is a utils.SudoCommand stub that records every invocation and
// returns canned stdout for `systemctl is-active`. Tests that need a dead
// daemon override the activeOutput.
type recordingSudo struct {
	runs         [][]string
	activeOutput string
}

func (r *recordingSudo) stub(name string, args ...string) *exec.Cmd {
	r.runs = append(r.runs, append([]string{name}, args...))
	if name == "systemctl" && len(args) >= 1 && args[0] == "is-active" {
		out := r.activeOutput
		if out == "" {
			out = "active\n"
		}
		return exec.Command("printf", "%s", out)
	}
	return exec.Command("true")
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

	// Point the NB-socket gate at a non-existent path so this test runs the
	// worker path (no ovn-nbctl). TestEnableOVNIPSec_Management covers the
	// management-node branch.
	origNBSock := ovnNBSocketPath
	ovnNBSocketPath = filepath.Join(configDir, "no-such-socket")
	t.Cleanup(func() { ovnNBSocketPath = origNBSock })

	d := &Daemon{configPath: configPath, clusterConfig: multiNodeClusterConfig()}
	require.NoError(t, d.enableOVNIPSec())

	// Expected sudo invocations in order:
	//   systemctl is-active openvswitch-ipsec.service     (unit enabled at provision time)
	//   ovs-vsctl set Open_vSwitch . other_config:certificate=... private_key=... ca_cert=...
	//   ovs-vsctl set Open_vSwitch . other_config:ipsec_encapsulation=true
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

	// Simulate a local NB socket so enableOVNIPSec runs the management
	// branch and writes NB_Global.ipsec=true.
	sockPath := filepath.Join(configDir, "ovnnb_db.sock")
	require.NoError(t, os.WriteFile(sockPath, []byte{}, 0600))
	origNBSock := ovnNBSocketPath
	ovnNBSocketPath = sockPath
	t.Cleanup(func() { ovnNBSocketPath = origNBSock })

	d := &Daemon{configPath: configPath, clusterConfig: multiNodeClusterConfig()}
	require.NoError(t, d.enableOVNIPSec())

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

	d := &Daemon{
		configPath: configPath,
		clusterConfig: &config.ClusterConfig{
			Node:  "node1",
			Nodes: map[string]config.Config{"node1": {}},
		},
	}
	require.NoError(t, d.enableOVNIPSec())
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

	d := &Daemon{configPath: configPath, clusterConfig: multiNodeClusterConfig()}
	err := d.enableOVNIPSec()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-monitor-ipsec")
	assert.Contains(t, err.Error(), "not active")

	// ovs-vsctl must NOT have been called — flipping ipsec_encapsulation=true
	// with no daemon is the silent-drop trap this guard exists to prevent.
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

	d := &Daemon{configPath: configPath, clusterConfig: multiNodeClusterConfig()}
	err := d.enableOVNIPSec()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing IPsec credential")
}

func TestEnableOVNIPSec_NoConfigPath(t *testing.T) {
	d := &Daemon{}
	err := d.enableOVNIPSec()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config path unset")
}
