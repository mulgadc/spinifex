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
