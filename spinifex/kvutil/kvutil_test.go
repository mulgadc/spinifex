package kvutil

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startJetStream starts an embedded JetStream-enabled NATS server and returns a
// handle on the jetstream package API.
func startJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return testutil.NewJetStream(t, nc)
}

// streamReplicas returns the replica count of the JetStream stream backing a KV
// bucket, so tests can assert on the config actually sent to the server.
func streamReplicas(t *testing.T, js jetstream.JetStream, bucket string) int {
	t.Helper()
	stream, err := js.Stream(t.Context(), "KV_"+bucket)
	require.NoError(t, err)
	info, err := stream.Info(t.Context())
	require.NoError(t, err)
	return info.Config.Replicas
}

func TestGetOrCreateBucket_CreatesAtDefaultReplicas(t *testing.T) {
	js := startJetStream(t)

	kv, err := GetOrCreateBucket(t.Context(), js, "regression-bucket", 5)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "regression-bucket"))
}

// TestGetOrCreateBucket_OpensExisting covers the second-boot path: a bucket that
// already exists is opened with its stored contents rather than reset.
func TestGetOrCreateBucket_OpensExisting(t *testing.T) {
	js := startJetStream(t)

	kv, err := GetOrCreateBucket(t.Context(), js, "existing-bucket", 5)
	require.NoError(t, err)
	_, err = kv.PutString(t.Context(), "survivor", "value")
	require.NoError(t, err)

	// A differing history must not stop the reopen — the existing config wins.
	reopened, err := GetOrCreateBucket(t.Context(), js, "existing-bucket", 1)
	require.NoError(t, err)
	entry, err := reopened.Get(t.Context(), "survivor")
	require.NoError(t, err)
	assert.Equal(t, "value", string(entry.Value()))
}

func TestGetOrCreateBucketWithReplicas_ClampsBelowOne(t *testing.T) {
	js := startJetStream(t)

	kv, err := GetOrCreateBucketWithReplicas(t.Context(), js, "clamped-zero", 1, 0)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "clamped-zero"))

	kv, err = GetOrCreateBucketWithReplicas(t.Context(), js, "clamped-negative", 1, -3)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "clamped-negative"))
}

// TestGetOrCreateBucketWithReplicas_SurfacesCreateFailure pins the reason the
// open is scoped to "bucket exists": a create that fails for any other reason
// must report that reason, not the "bucket not found" a blind reopen produces.
func TestGetOrCreateBucketWithReplicas_SurfacesCreateFailure(t *testing.T) {
	js := startJetStream(t)

	// The embedded single-node server rejects Replicas > 1.
	_, err := GetOrCreateBucketWithReplicas(t.Context(), js, "over-replicated", 1, 3)
	require.Error(t, err)
	assert.NotErrorIs(t, err, jetstream.ErrBucketNotFound)
	assert.Contains(t, err.Error(), "create KV bucket over-replicated")
}

func TestDeleteBucketIfExists(t *testing.T) {
	js := startJetStream(t)

	// Missing bucket is a no-op.
	require.NoError(t, DeleteBucketIfExists(t.Context(), js, "ghost-bucket"))

	// Existing bucket gets deleted.
	_, err := GetOrCreateBucket(t.Context(), js, "doomed-bucket", 1)
	require.NoError(t, err)
	require.NoError(t, DeleteBucketIfExists(t.Context(), js, "doomed-bucket"))

	_, err = js.KeyValue(t.Context(), "doomed-bucket")
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "bucket should be gone")

	// Calling again on the now-missing bucket is still a no-op (idempotent).
	require.NoError(t, DeleteBucketIfExists(t.Context(), js, "doomed-bucket"))
}

func TestBucketNames_ListsEveryBucket(t *testing.T) {
	js := startJetStream(t)

	names, err := BucketNames(t.Context(), js)
	require.NoError(t, err)
	assert.Empty(t, names, "no buckets yet")

	for _, bucket := range []string{"names-alpha", "names-beta"} {
		_, err := GetOrCreateBucket(t.Context(), js, bucket, 1)
		require.NoError(t, err)
	}

	names, err = BucketNames(t.Context(), js)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"names-alpha", "names-beta"}, names, "names are unprefixed bucket names")
}

// TestBucketNames_SurfacesEnumerationFailure is the reason this helper exists:
// the underlying lister closes its channel on failure exactly as it does on
// success, so a caller that ignores Error() sees a failed listing as an empty
// one and prunes every resource it was meant to keep.
func TestBucketNames_SurfacesEnumerationFailure(t *testing.T) {
	js := startJetStream(t)

	_, err := GetOrCreateBucket(t.Context(), js, "should-not-vanish", 1)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	names, err := BucketNames(ctx, js)
	require.Error(t, err, "a failed listing must not read as a complete one")
	assert.Nil(t, names)
}

func TestKeys_ListsKeysAndSurfacesCancellation(t *testing.T) {
	js := startJetStream(t)
	kv, err := GetOrCreateBucket(t.Context(), js, "bounded-keys", 1)
	require.NoError(t, err)
	_, err = kv.PutString(t.Context(), "key", "value")
	require.NoError(t, err)

	keys, err := Keys(t.Context(), kv)
	require.NoError(t, err)
	require.Equal(t, []string{"key"}, keys)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	keys, err = Keys(ctx, kv)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, keys)
}

// TestVersionStateMachine covers unset→0, first write, idempotent same write, upgrade, and no-downgrade.
// One bucket is reused so each step runs against the prior state — the only way to catch unconditional-overwrite regressions.
func TestVersionStateMachine(t *testing.T) {
	js := startJetStream(t)
	kv, err := GetOrCreateBucket(t.Context(), js, "test-version-fsm", 1)
	require.NoError(t, err)

	// Unset → 0.
	v, err := ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, 0, v, "ReadVersion on unset bucket")

	steps := []struct {
		name  string
		write int
		want  int // expected ReadVersion after the write
	}{
		{"first write persists", 1, 1},
		{"same version is no-op", 1, 1},
		{"higher version upgrades", 2, 2},
		{"lower version is no-op (no downgrade)", 1, 2},
		{"larger jump upgrades", 5, 5},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			require.NoError(t, WriteVersion(t.Context(), kv, step.write))
			v, err := ReadVersion(t.Context(), kv)
			require.NoError(t, err)
			assert.Equal(t, step.want, v)
		})
	}

	// Round-trip the raw KV value to confirm the encoding is what readers
	// outside this package would expect.
	entry, err := kv.Get(t.Context(), utils.VersionKey)
	require.NoError(t, err)
	assert.Equal(t, "5", string(entry.Value()))
}

// TestVersionCorruptValue asserts a mangled stamp is reported rather than
// silently overwritten, which would hide that the bucket needs inspection.
func TestVersionCorruptValue(t *testing.T) {
	js := startJetStream(t)
	kv, err := GetOrCreateBucket(t.Context(), js, "test-version-corrupt", 1)
	require.NoError(t, err)
	_, err = kv.PutString(t.Context(), utils.VersionKey, "not-a-number")
	require.NoError(t, err)

	_, err = ReadVersion(t.Context(), kv)
	require.ErrorContains(t, err, "corrupted")

	require.ErrorContains(t, WriteVersion(t.Context(), kv, 2), "corrupted")
}
