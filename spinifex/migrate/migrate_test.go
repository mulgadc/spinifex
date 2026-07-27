package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestNATS starts an embedded NATS server with JetStream for testing.
func startTestNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	ns, nc, _ := testutil.StartTestJetStream(t)
	return ns, nc
}

func createTestBucket(t *testing.T, nc *nats.Conn, name string) jetstream.KeyValue {
	t.Helper()
	js := testutil.NewJetStream(t, nc)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: name, History: 1})
	require.NoError(t, err)
	return kv
}

// --- Registry validation tests ---

func TestRegistry_ValidatesChainNoGaps(t *testing.T) {
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{FromVersion: 1, ToVersion: 2, Description: "first", Run: func(context.Context, KVContext) error { return nil }})
	r.RegisterKV("test-bucket", KVMigration{FromVersion: 3, ToVersion: 4, Description: "gap", Run: func(context.Context, KVContext) error { return nil }})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	// Stamp version 1 to simulate existing bucket.
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 4)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gap")
}

func TestRegistry_RejectsDuplicateVersions(t *testing.T) {
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{FromVersion: 1, ToVersion: 2, Description: "first", Run: func(context.Context, KVContext) error { return nil }})
	r.RegisterKV("test-bucket", KVMigration{FromVersion: 1, ToVersion: 2, Description: "duplicate", Run: func(context.Context, KVContext) error { return nil }})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	// Two migrations with the same FromVersion — chain validation will catch this
	// because after running first 1→2, the second 1→2 won't match expected=2.
	err = r.RunKV(t.Context(), "test-bucket", kv, 2)
	// Should succeed (first 1→2 runs, second is filtered out since FromVersion < current after first runs).
	// Actually with our filtering, both have FromVersion=1 >= current=1 and ToVersion=2 <= target=2,
	// so both are in pending. Chain validation: expected=1, first.From=1 ✓, expected=2, second.From=1 ✗.
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gap")
}

// --- RunKV tests ---

func TestRunKV_NoPendingMigrations_NoOp(t *testing.T) {
	r := NewRegistry()
	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")

	// Stamp version 1 directly.
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	// RunKV with target=1, current=1 → no-op.
	err = r.RunKV(t.Context(), "test-bucket", kv, 1)
	assert.NoError(t, err)
}

func TestRunKV_FreshBucket_WithMigrations(t *testing.T) {
	ran := false
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "first kv migration",
		Run: func(context.Context, KVContext) error { ran = true; return nil },
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")

	// Fresh bucket (no _version key), targetVersion=2. The chain bottoms at
	// 1, so RunKV should accept the chain and run it without a "gap" error.
	err := r.RunKV(t.Context(), "test-bucket", kv, 2)
	require.NoError(t, err)
	assert.True(t, ran, "migration should run on fresh bucket with registered chain")

	entry, err := kv.Get(t.Context(), "_version")
	require.NoError(t, err)
	assert.Equal(t, "2", string(entry.Value()))
}

func TestRunKV_FreshBucket_StampsVersion(t *testing.T) {
	r := NewRegistry()
	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")

	// No version set → version 0 (fresh bucket).
	err := r.RunKV(t.Context(), "test-bucket", kv, 1)
	assert.NoError(t, err)

	// Verify version was stamped.
	entry, err := kv.Get(t.Context(), "_version")
	require.NoError(t, err)
	assert.Equal(t, "1", string(entry.Value()))
}

func TestRunKV_ExecutesMigrationsInOrder(t *testing.T) {
	var order []int
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "step 1",
		Run: func(context.Context, KVContext) error { order = append(order, 1); return nil },
	})
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 2, ToVersion: 3, Description: "step 2",
		Run: func(context.Context, KVContext) error { order = append(order, 2); return nil },
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 3)
	assert.NoError(t, err)
	assert.Equal(t, []int{1, 2}, order)

	// Verify final version.
	entry, err := kv.Get(t.Context(), "_version")
	require.NoError(t, err)
	assert.Equal(t, "3", string(entry.Value()))
}

func TestRunKV_StampsAfterEachStep(t *testing.T) {
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "step 1",
		Run: func(context.Context, KVContext) error {
			// After this runs, version should be stamped to 2 by RunKV.
			return nil
		},
	})
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 2, ToVersion: 3, Description: "step 2 fails",
		Run: func(context.Context, KVContext) error { return errors.New("boom") },
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 3)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "boom")

	// Version should be 2 (stamped after step 1, before step 2 failed).
	entry, err := kv.Get(t.Context(), "_version")
	require.NoError(t, err)
	assert.Equal(t, "2", string(entry.Value()))
}

func TestRunKV_StopsOnFailure_VersionNotBumped(t *testing.T) {
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "fails",
		Run: func(context.Context, KVContext) error { return errors.New("migration error") },
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 2)
	assert.Error(t, err)

	// Version remains at 1.
	entry, err := kv.Get(t.Context(), "_version")
	require.NoError(t, err)
	assert.Equal(t, "1", string(entry.Value()))
}

func TestRunKV_RejectsMissingMigration(t *testing.T) {
	r := NewRegistry()
	// No migrations registered, but bucket is at version 1 and target is 2.

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no migrations registered")
}

func TestRunKV_Idempotent(t *testing.T) {
	runCount := 0
	r := NewRegistry()
	r.RegisterKV("test-bucket", KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "add field",
		Run: func(ctx context.Context, kvc KVContext) error {
			runCount++
			// Idempotent: write a key, re-running writes the same value.
			_, err := kvc.KV.PutString(ctx, "data.key1", `{"field":"value"}`)
			return err
		},
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "1")
	require.NoError(t, err)

	// First run.
	err = r.RunKV(t.Context(), "test-bucket", kv, 2)
	assert.NoError(t, err)
	assert.Equal(t, 1, runCount)

	// Second run — already at version 2, should be no-op.
	err = r.RunKV(t.Context(), "test-bucket", kv, 2)
	assert.NoError(t, err)
	assert.Equal(t, 1, runCount) // Not incremented.
}

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
