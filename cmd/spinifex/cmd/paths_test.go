package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withProductionMarker temporarily overrides the marker paths used to detect a
// production install. Tests that pass non-existent paths force "dev" mode; tests
// that pass a real path such as t.TempDir() force "production" mode.
func withProductionMarker(t *testing.T, paths ...string) {
	t.Helper()
	orig := productionMarkerPaths
	productionMarkerPaths = paths
	t.Cleanup(func() { productionMarkerPaths = orig })
}

func TestIsProductionLayout(t *testing.T) {
	t.Run("absent marker returns false", func(t *testing.T) {
		withProductionMarker(t, filepath.Join(t.TempDir(), "does-not-exist"))
		assert.False(t, isProductionLayout())
	})
	t.Run("present marker returns true", func(t *testing.T) {
		withProductionMarker(t, t.TempDir())
		assert.True(t, isProductionLayout())
	})
	// A node reset clears /etc/spinifex but leaves the systemd units installed.
	// If that combination reads as "dev", init rebuilds the node under the
	// invoking user's home while the units keep reading /etc/spinifex.
	t.Run("surviving marker keeps production layout after a reset", func(t *testing.T) {
		unit := filepath.Join(t.TempDir(), "spinifex.target")
		require.NoError(t, os.WriteFile(unit, nil, 0600))
		withProductionMarker(t, unit, filepath.Join(t.TempDir(), "etc-spinifex-removed"))

		assert.True(t, isProductionLayout())
		assert.Equal(t, "/etc/spinifex", DefaultConfigDir())
		assert.Equal(t, "/var/lib/spinifex", DefaultDataDir())
	})
	t.Run("no markers at all returns false", func(t *testing.T) {
		dir := t.TempDir()
		withProductionMarker(t, filepath.Join(dir, "no-unit"), filepath.Join(dir, "no-etc"))
		assert.False(t, isProductionLayout())
	})
}

func TestEnsureConfigDir(t *testing.T) {
	// Outside a production install a missing config dir is ordinary: a dev tree
	// is built on demand under the user's home.
	t.Run("creates a missing dev directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "config")
		require.NoError(t, EnsureConfigDir(dir))

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})
	// A deploy may legitimately widen the mode of an existing config dir, so
	// re-init must leave one it did not create alone.
	t.Run("leaves an existing directory untouched", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0755))
		require.NoError(t, EnsureConfigDir(dir))

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
	})
	t.Run("reports the error when the path is unusable", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(file, nil, 0600))
		require.Error(t, EnsureConfigDir(filepath.Join(file, "config")))
	})
	// The regression: init used to reroute to ~/spinifex and report success,
	// leaving the units reading a /etc/spinifex that was never rebuilt. Only
	// setup.sh can restore the per-service subtree, so name it and stop.
	t.Run("refuses when a production node has lost its config dir", func(t *testing.T) {
		dir := t.TempDir()
		unit := filepath.Join(dir, "spinifex.target")
		require.NoError(t, os.WriteFile(unit, nil, 0600))
		withProductionMarker(t, unit)

		missing := filepath.Join(dir, "etc-spinifex-removed")
		orig := productionConfigDir
		productionConfigDir = missing
		t.Cleanup(func() { productionConfigDir = orig })

		err := EnsureConfigDir(missing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "re-run setup.sh")
		assert.NoDirExists(t, missing)
	})
}

func TestDefaultPaths(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("SUDO_USER", "")

	tests := []struct {
		name           string
		productionMode bool
		wantConfigDir  string
		wantDataDir    string
		wantConfigFile string
		wantLogDir     string // for LogDirFor("/data/dir")
	}{
		{
			name:           "dev layout",
			productionMode: false,
			wantConfigDir:  filepath.Join(homeDir, "spinifex", "config"),
			wantDataDir:    filepath.Join(homeDir, "spinifex"),
			wantConfigFile: filepath.Join(homeDir, "spinifex", "config", "spinifex.toml"),
			wantLogDir:     "/data/dir/logs",
		},
		{
			name:           "production layout",
			productionMode: true,
			wantConfigDir:  "/etc/spinifex",
			wantDataDir:    "/var/lib/spinifex",
			wantConfigFile: "/etc/spinifex/spinifex.toml",
			wantLogDir:     "/var/log/spinifex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.productionMode {
				withProductionMarker(t, t.TempDir())
			} else {
				withProductionMarker(t, filepath.Join(t.TempDir(), "absent"))
			}

			assert.Equal(t, tt.wantConfigDir, DefaultConfigDir())
			assert.Equal(t, tt.wantDataDir, DefaultDataDir())
			assert.Equal(t, tt.wantConfigFile, DefaultConfigFile())
			assert.Equal(t, tt.wantLogDir, LogDirFor("/data/dir"))
		})
	}
}
