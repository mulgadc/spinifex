package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Config version reader tests ---

func TestTOMLVersionReader_ReadStringVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(path, []byte(`version = "1.0"
[server]
port = 8080
`), 0o644))

	r := &TOMLVersionReader{}
	v, err := r.ReadVersion(path)
	require.NoError(t, err)
	assert.Equal(t, 1, v)
}

func TestTOMLVersionReader_ReadIntVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`version = 2
[server]
port = 8080
`), 0o644))

	r := &TOMLVersionReader{}
	v, err := r.ReadVersion(path)
	require.NoError(t, err)
	assert.Equal(t, 2, v)
}

func TestTOMLVersionReader_WriteVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`version = "1.0"
[server]
port = 8080
`), 0o644))

	r := &TOMLVersionReader{}
	require.NoError(t, r.WriteVersion(path, 2))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `version = "2"`)
	assert.NotContains(t, string(data), `"1.0"`)
	assert.Contains(t, string(data), "port = 8080") // rest preserved
}

func TestNATSConfVersionReader_ReadNoMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nats.conf")
	require.NoError(t, os.WriteFile(path, []byte(`# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o644))

	r := &NATSConfVersionReader{}
	v, err := r.ReadVersion(path)
	require.NoError(t, err)
	assert.Equal(t, 0, v)
}

func TestNATSConfVersionReader_ReadWithMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nats.conf")
	require.NoError(t, os.WriteFile(path, []byte(`# spinifex-config-version: 1
# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o644))

	r := &NATSConfVersionReader{}
	v, err := r.ReadVersion(path)
	require.NoError(t, err)
	assert.Equal(t, 1, v)
}

func TestNATSConfVersionReader_WriteVersion_Prepend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nats.conf")
	require.NoError(t, os.WriteFile(path, []byte(`# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o644))

	r := &NATSConfVersionReader{}
	require.NoError(t, r.WriteVersion(path, 1))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(string(data), "\n")
	assert.Equal(t, "# spinifex-config-version: 1", lines[0])
	assert.Equal(t, "# NATS Server Configuration", lines[1])
}

func TestNATSConfVersionReader_WriteVersion_Replace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nats.conf")
	require.NoError(t, os.WriteFile(path, []byte(`# spinifex-config-version: 1
# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o644))

	r := &NATSConfVersionReader{}
	require.NoError(t, r.WriteVersion(path, 2))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(string(data), "\n")
	assert.Equal(t, "# spinifex-config-version: 2", lines[0])
	assert.Equal(t, "# NATS Server Configuration", lines[1])
}

// --- Config migration tests ---

func TestRunConfig_CreatesBackup(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# NATS Server Configuration
authorization {
#  token: "testtoken123"
}
`), 0o640))

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0, ToVersion: 1, Description: "test migration",
		Run: func(ctx ConfigContext) error {
			return nil // no-op for backup test
		},
	})

	err := r.RunConfig("nats.conf", dir, dir)
	require.NoError(t, err)

	// Check backup exists.
	entries, err := os.ReadDir(natsDir)
	require.NoError(t, err)
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "nats.conf.pre-migrate-0to1.") {
			found = true
			break
		}
	}
	assert.True(t, found, "backup file should exist")
}

func TestRunConfig_SkipsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	// No nats.conf file exists — fresh install.

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0, ToVersion: 1, Description: "should not run",
		Run: func(ctx ConfigContext) error {
			t.Fatal("migration should not run on fresh install")
			return nil
		},
	})

	err := r.RunConfig("nats.conf", dir, dir)
	assert.NoError(t, err)
}

func TestRunConfig_AlreadyMigrated_NoOp(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# spinifex-config-version: 1
# NATS Server Configuration
authorization {
  token: "testtoken123"
}
`), 0o640))

	ran := false
	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0, ToVersion: 1, Description: "already done",
		Run: func(ctx ConfigContext) error { ran = true; return nil },
	})

	err := r.RunConfig("nats.conf", dir, dir)
	assert.NoError(t, err)
	assert.False(t, ran)
}

// --- BackupConfig tests ---

func TestBackupConfig_CreatesTimestampedBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")
	content := []byte("original content")
	require.NoError(t, os.WriteFile(path, content, 0o640))

	backupPath, err := BackupConfig(path, 0, 1)
	require.NoError(t, err)
	assert.Contains(t, backupPath, "pre-migrate-0to1")

	backupData, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, content, backupData)
}

// --- PendingConfig tests ---

func TestPendingConfig_ReturnsPending(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# NATS Server Configuration
authorization {
#  token: "testtoken"
}
`), 0o640))

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0,
		ToVersion:   1,
		Description: "Enable NATS authorization token",
		Run:         func(ConfigContext) error { return nil },
	})

	pending, err := r.PendingConfig(dir)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "nats.conf", pending[0].Target)
	assert.Equal(t, 0, pending[0].FromVersion)
	assert.Equal(t, 1, pending[0].ToVersion)
}

func TestPendingConfig_NoPending(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# spinifex-config-version: 1
# NATS Server Configuration
`), 0o640))

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0,
		ToVersion:   1,
		Description: "already done",
		Run:         func(ConfigContext) error { return nil },
	})

	pending, err := r.PendingConfig(dir)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// --- Config migration error and chain tests ---

func TestRunConfig_StopsOnFailure_VersionNotBumped(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o640))

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0,
		ToVersion:   1,
		Description: "fails",
		Run:         func(ConfigContext) error { return errors.New("migration error") },
	})

	err := r.RunConfig("nats.conf", dir, dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "migration error")

	// Version should remain at 0 (no version marker).
	reader := &NATSConfVersionReader{}
	v, err := reader.ReadVersion(confPath)
	require.NoError(t, err)
	assert.Equal(t, 0, v)
}

func TestRunConfig_ExecutesMultiStepChainInOrder(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o640))

	var order []int
	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0, ToVersion: 1, Description: "step 1",
		Run: func(ConfigContext) error { order = append(order, 1); return nil },
	})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 1, ToVersion: 2, Description: "step 2",
		Run: func(ConfigContext) error { order = append(order, 2); return nil },
	})

	err := r.RunConfig("nats.conf", dir, dir)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, order)

	// Verify final version is 2.
	reader := &NATSConfVersionReader{}
	v, err := reader.ReadVersion(confPath)
	require.NoError(t, err)
	assert.Equal(t, 2, v)
}

func TestRunConfig_RejectsChainGap(t *testing.T) {
	dir := t.TempDir()
	natsDir := filepath.Join(dir, "nats")
	require.NoError(t, os.MkdirAll(natsDir, 0o755))
	confPath := filepath.Join(natsDir, "nats.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(`# NATS Server Configuration
listen: 0.0.0.0:4222
`), 0o640))

	r := NewRegistry()
	r.RegisterConfigTarget("nats.conf", "nats/nats.conf", &NATSConfVersionReader{})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 0, ToVersion: 1, Description: "step 1",
		Run: func(ConfigContext) error { return nil },
	})
	r.RegisterConfig("nats.conf", ConfigMigration{
		FromVersion: 3, ToVersion: 4, Description: "gap",
		Run: func(ConfigContext) error { return nil },
	})

	err := r.RunConfig("nats.conf", dir, dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gap")
}
