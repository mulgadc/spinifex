package cmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFirewallHelper points the firewall helper at a script that logs its
// arguments, and returns the path of that log.
func stubFirewallHelper(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	helper := filepath.Join(dir, "spinifex-firewall-apply")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + body
	require.NoError(t, os.WriteFile(helper, []byte(script), 0o755))
	t.Cleanup(cmd.SetFirewallApplyHelper(helper))
	return log
}

func helperCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(strings.TrimSpace(strings.ReplaceAll(string(data), "\n", " ")))
}

func TestOpenFormationPort(t *testing.T) {
	log := stubFirewallHelper(t, "exit 0\n")

	closePort := cmd.OpenFormationPort(4432)
	assert.Equal(t, []string{"open-port", "4432"}, helperCalls(t, log))

	closePort()
	assert.Equal(t, []string{"open-port", "4432", "close-port", "4432"}, helperCalls(t, log),
		"the window must be closed again once formation ends")
}

// A machine with no policy installed has nothing to open, and formation must not
// be held up by looking for a helper that was never installed.
func TestOpenFormationPort_NoHelperInstalled(t *testing.T) {
	t.Cleanup(cmd.SetFirewallApplyHelper(filepath.Join(t.TempDir(), "absent")))

	closePort := cmd.OpenFormationPort(4432)
	require.NotNil(t, closePort)
	closePort()
}

// Opening is best effort: an operator forming a cluster must not be blocked by a
// firewall helper that fails. The close must then not run either, since there is
// nothing open to close.
func TestOpenFormationPort_FailureDoesNotBlockFormation(t *testing.T) {
	log := stubFirewallHelper(t, "exit 1\n")

	closePort := cmd.OpenFormationPort(4432)
	require.NotNil(t, closePort)
	closePort()

	assert.Equal(t, []string{"open-port", "4432"}, helperCalls(t, log),
		"a failed open must not be followed by a close")
}
