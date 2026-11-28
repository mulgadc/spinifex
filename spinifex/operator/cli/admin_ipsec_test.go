package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeToml(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spinifex.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// spx admin init --force without --ipsec must not change an existing
// cluster's encryption in either direction.
func TestResolveInitIPSec_ReInitKeepsExistingSetting(t *testing.T) {
	on := writeToml(t, "[network]\nipsec_enabled = true\n")
	off := writeToml(t, "[network]\nipsec_enabled = false\n")

	got, err := resolveInitIPSec(false, false, on)
	require.NoError(t, err)
	assert.True(t, got, "an encrypted cluster stays encrypted")

	got, err = resolveInitIPSec(false, false, off)
	require.NoError(t, err)
	assert.False(t, got, "an unencrypted cluster stays unencrypted")
}

func TestResolveInitIPSec_ExplicitFlagWins(t *testing.T) {
	on := writeToml(t, "[network]\nipsec_enabled = true\n")
	got, err := resolveInitIPSec(false, true, on)
	require.NoError(t, err)
	assert.False(t, got)

	off := writeToml(t, "[network]\nipsec_enabled = false\n")
	got, err = resolveInitIPSec(true, true, off)
	require.NoError(t, err)
	assert.True(t, got)
}

func TestResolveInitIPSec_NewInstallTakesDefault(t *testing.T) {
	got, err := resolveInitIPSec(false, false, filepath.Join(t.TempDir(), "spinifex.toml"))
	require.NoError(t, err)
	assert.False(t, got)
}

// A config written before the key existed reads as encrypted, as the daemon
// reads it, so a re-init does not turn its encryption off.
func TestResolveInitIPSec_KeylessConfigKeepsEncryption(t *testing.T) {
	got, err := resolveInitIPSec(false, false, writeToml(t, "[network]\n"))
	require.NoError(t, err)
	assert.True(t, got)
}

func TestResolveInitIPSec_UnreadableConfigIsAnError(t *testing.T) {
	_, err := resolveInitIPSec(false, false, writeToml(t, "[network\nipsec_enabled = "))
	assert.ErrorContains(t, err, "parse the existing config")
}
