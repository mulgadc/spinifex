package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgradeSeesEveryConfigTarget pins the registrations `spx admin upgrade`
// depends on. A migration whose package is no longer linked here would leave
// the command reporting nothing pending on an install that needs migrating.
//
// spinifex.toml is the only config target: predastore.toml is not migrated at
// all, so a predastore config on disk must contribute neither a version nor a
// pending step.
func TestUpgradeSeesEveryConfigTarget(t *testing.T) {
	configDir := t.TempDir()

	// A target must exist on disk to be reported at all.
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "predastore"), 0750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"3\"\n"), 0640))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "predastore", "predastore.toml"), []byte("version = 1\n"), 0640))

	versions, err := migrate.DefaultRegistry.ConfigVersions(configDir)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"spinifex.toml": 3}, versions)

	pending, err := migrate.DefaultRegistry.PendingConfig(configDir)
	require.NoError(t, err)

	targets := make(map[string][2]int, len(pending))
	for _, p := range pending {
		targets[p.Target] = [2]int{p.FromVersion, p.ToVersion}
	}
	assert.Equal(t, map[string][2]int{"spinifex.toml": {3, 4}}, targets)
}
