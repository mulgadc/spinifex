package migrate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	statemigrate "github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{FromVersion: 1, ToVersion: 2, Description: "first", Run: func(context.Context, statemigrate.KVContext) error { return nil }})
	r.RegisterKV("test-bucket", statemigrate.KVMigration{FromVersion: 3, ToVersion: 4, Description: "gap", Run: func(context.Context, statemigrate.KVContext) error { return nil }})

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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{FromVersion: 1, ToVersion: 2, Description: "first", Run: func(context.Context, statemigrate.KVContext) error { return nil }})
	r.RegisterKV("test-bucket", statemigrate.KVMigration{FromVersion: 1, ToVersion: 2, Description: "duplicate", Run: func(context.Context, statemigrate.KVContext) error { return nil }})

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
	r := statemigrate.NewRegistry()
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "first kv migration",
		Run: func(context.Context, statemigrate.KVContext) error { ran = true; return nil },
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
	r := statemigrate.NewRegistry()
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "step 1",
		Run: func(context.Context, statemigrate.KVContext) error { order = append(order, 1); return nil },
	})
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 2, ToVersion: 3, Description: "step 2",
		Run: func(context.Context, statemigrate.KVContext) error { order = append(order, 2); return nil },
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "step 1",
		Run: func(context.Context, statemigrate.KVContext) error {
			// After this runs, version should be stamped to 2 by RunKV.
			return nil
		},
	})
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 2, ToVersion: 3, Description: "step 2 fails",
		Run: func(context.Context, statemigrate.KVContext) error { return errors.New("boom") },
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "fails",
		Run: func(context.Context, statemigrate.KVContext) error { return errors.New("migration error") },
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
	r := statemigrate.NewRegistry()
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
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "add field",
		Run: func(ctx context.Context, kvc statemigrate.KVContext) error {
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

// A bucket stamped past what this build understands was migrated by a node on
// a newer release. Opening it anyway is the mixed-cluster failure: this build
// reads the keys it knows, misses the ones it does not, and writes back a view
// of the bucket assembled from half of it.
func TestRunKV_SchemaAheadOfThisBuildIsRefused(t *testing.T) {
	r := statemigrate.NewRegistry()
	r.RegisterKV("test-bucket", statemigrate.KVMigration{
		FromVersion: 1, ToVersion: 2, Description: "add field",
		Run: func(ctx context.Context, kvc statemigrate.KVContext) error { return nil },
	})

	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, "test-bucket")
	_, err := kv.PutString(t.Context(), "_version", "5")
	require.NoError(t, err)

	err = r.RunKV(t.Context(), "test-bucket", kv, 2)

	var ahead statemigrate.SchemaAheadError
	require.ErrorAs(t, err, &ahead)
	assert.Equal(t, 5, ahead.Found)
	assert.Equal(t, 2, ahead.Understood)
	assert.Contains(t, err.Error(), "test-bucket")
	assert.Contains(t, err.Error(), "upgrade this node",
		"the error has to say what to do about it, not just that it happened")
}
