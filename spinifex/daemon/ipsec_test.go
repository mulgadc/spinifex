package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnableOVNIPSec(t *testing.T) {
	// Swap sudoCommand for a stub that records args and returns nil.
	type recorderture struct {
		argsRuns [][]string
	}
	recorder := &recorderture{}
	orig := sudoCommand
	sudoCommand = func(name string, args ...string) *exec.Cmd {
		recorder.argsRuns = append(recorder.argsRuns, append([]string{name}, args...))
		return exec.Command("true")
	}
	t.Cleanup(func() { sudoCommand = orig })

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))

	// Seed the three credential files enableOVNIPSec checks for. Bytes
	// don't matter — only the presence stat.
	for _, rel := range []string{"ca.pem", "ipsec/peer.pem", "ipsec/peer.key"} {
		full := filepath.Join(configDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0750))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0600))
	}

	d := &Daemon{configPath: configPath}
	require.NoError(t, d.enableOVNIPSec())

	// Two ovs-vsctl invocations: cert pointers + ipsec_enrecordersulation toggle.
	require.Len(t, recorder.argsRuns, 2)
	for _, run := range recorder.argsRuns {
		assert.Equal(t, "ovs-vsctl", run[0])
		assert.Equal(t, "set", run[1])
		assert.Equal(t, "Open_vSwitch", run[2])
	}
	joined := strings.Join(recorder.argsRuns[0], " ")
	assert.Contains(t, joined, "other_config:certificate="+filepath.Join(configDir, "ipsec", "peer.pem"))
	assert.Contains(t, joined, "other_config:private_key="+filepath.Join(configDir, "ipsec", "peer.key"))
	assert.Contains(t, joined, "other_config:ca_cert="+filepath.Join(configDir, "ca.pem"))
	assert.Contains(t, strings.Join(recorder.argsRuns[1], " "), "other_config:ipsec_encapsulation=true")
}

func TestEnableOVNIPSec_MissingCert(t *testing.T) {
	orig := sudoCommand
	sudoCommand = func(name string, args ...string) *exec.Cmd {
		t.Fatalf("sudoCommand must not run when cert files are absent")
		return exec.Command("true")
	}
	t.Cleanup(func() { sudoCommand = orig })

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "spinifex.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("placeholder"), 0600))
	// CA exists; peer cert missing.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "ca.pem"), []byte("x"), 0600))

	d := &Daemon{configPath: configPath}
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
