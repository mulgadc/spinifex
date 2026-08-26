package gateway_bedrock

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopWeightsResolver_ResolvesNothing(t *testing.T) {
	snapshotID, ok, err := NoopWeightsResolver.Resolve(context.Background(), "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, snapshotID)
}

// TestWeightsStore_ResolveMiss covers a KV miss on a model with no staged
// snapshot: ("", false, nil), not an error.
func TestWeightsStore_ResolveMiss(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	snapshotID, ok, err := store.Resolve(context.Background(), "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, snapshotID)
}

// TestWeightsStore_PutAndResolve_KV exercises the real JetStream KV path:
// bucket (lazy create), weightsKey, PutWeights, and the KV-hit branch of
// Resolve.
func TestWeightsStore_PutAndResolve_KV(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))

	snapshotID, ok, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "snap-0001", snapshotID)
}

// TestWeightsStore_PutOverwritesPrevious covers a re-stage: PutWeights
// overwrites the prior record in place rather than erroring or accumulating
// history.
func TestWeightsStore_PutOverwritesPrevious(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b-v2/", "snap-0002"))

	snapshotID, ok, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "snap-0002", snapshotID)
}

// TestWeightsStore_GetWeights_Miss covers 'ochre weights stage' checking for
// a prior record before materialising anything: a never-staged model
// reports ok=false, not an error.
func TestWeightsStore_GetWeights_Miss(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	entry, ok, err := store.GetWeights(context.Background(), "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Zero(t, entry)
}

// TestWeightsStore_GetWeights_ReturnsSourceURIAndSnapshot covers the
// idempotency check stage relies on: the same source URI staged twice must
// be detectable as a no-op, and a different source must surface the
// snapshot it is about to replace.
func TestWeightsStore_GetWeights_ReturnsSourceURIAndSnapshot(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))

	entry, ok, err := store.GetWeights(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, WeightsEntry{
		ModelID:    "meta.llama3-2-1b-instruct-v1:0",
		SourceURI:  "s3://models/llama-3.2-1b/",
		SnapshotID: "snap-0001",
	}, entry)
}

// TestWeightsStore_PutWeightsWithRevision_RoundTrips covers D3: a pull-staged
// model's upstream commit SHA survives the KV round trip and comes back on
// both GetWeights and ListWeights.
func TestWeightsStore_PutWeightsWithRevision_RoundTrips(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeightsWithRevision(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://ochre-weights/meta-llama/Llama-3.2-1B-Instruct/abc123/", "snap-0001", "abc123def456"))

	entry, ok, err := store.GetWeights(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "abc123def456", entry.SourceRevision)

	entries, err := store.ListWeights(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "abc123def456", entries[0].SourceRevision)
}

// TestWeightsStore_PutWeights_LeavesSourceRevisionEmpty covers the offline
// stage path: weights staged with no pull manifest must record an empty
// SourceRevision, not fail or fabricate one.
func TestWeightsStore_PutWeights_LeavesSourceRevisionEmpty(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))

	entry, ok, err := store.GetWeights(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, entry.SourceRevision)
}

// TestWeightsStore_ListWeights_Empty covers the no-keys-yet case: an empty
// bucket must report an empty list, not an error (jetstream.ErrNoKeysFound).
func TestWeightsStore_ListWeights_Empty(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	entries, err := store.ListWeights(context.Background())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestWeightsStore_ListWeights_ReturnsAllStaged covers the operator-facing
// listing: every staged model comes back with its modelID decoded (not the
// base64url KV key), its source URI and its snapshot ID, sorted by model ID.
func TestWeightsStore_ListWeights_ReturnsAllStaged(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))
	require.NoError(t, store.PutWeights(ctx, "amazon.titan-text-lite-v1", "s3://models/titan-text-lite/", "snap-0002"))

	entries, err := store.ListWeights(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, []WeightsEntry{
		{ModelID: "amazon.titan-text-lite-v1", SourceURI: "s3://models/titan-text-lite/", SnapshotID: "snap-0002"},
		{ModelID: "meta.llama3-2-1b-instruct-v1:0", SourceURI: "s3://models/llama-3.2-1b/", SnapshotID: "snap-0001"},
	}, entries)
}

// TestWeightsStore_DeleteWeights_RemovesEntry covers the KV-only removal:
// DeleteWeights drops the record and Resolve reports not-found afterwards.
func TestWeightsStore_DeleteWeights_RemovesEntry(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/llama-3.2-1b/", "snap-0001"))
	require.NoError(t, store.DeleteWeights(ctx, "meta.llama3-2-1b-instruct-v1:0"))

	_, ok, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestWeightsStore_DeleteWeights_NeverStagedIsNotAnError covers 'ochre
// weights remove' against a model that was never staged: the CLI checks
// GetWeights first to print a friendly message, but DeleteWeights itself
// must not error on a missing key either.
func TestWeightsStore_DeleteWeights_NeverStagedIsNotAnError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	require.NoError(t, store.DeleteWeights(context.Background(), "meta.llama3-2-1b-instruct-v1:0"))
}

func TestWeightsKey(t *testing.T) {
	// base64url(RawURLEncoding) of "meta.llama3-2-1b-instruct-v1:0".
	assert.Equal(t, "bWV0YS5sbGFtYTMtMi0xYi1pbnN0cnVjdC12MTow", weightsKey("meta.llama3-2-1b-instruct-v1:0"))
}

func TestSetWeightsResolver_NilRestoresNoop(t *testing.T) {
	SetWeightsResolver(stubWeightsResolver{ok: map[string]bool{"x": true}})
	SetWeightsResolver(nil)
	t.Cleanup(func() { SetWeightsResolver(nil) })

	_, ok, err := currentWeightsResolver().Resolve(context.Background(), "x")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestWeightsStore_RequiresJetStream covers a store constructed with no
// JetStream client: every accessor must report the misconfiguration rather
// than panic on a nil handle.
func TestWeightsStore_RequiresJetStream(t *testing.T) {
	store := NewWeightsStore(nil, 1)
	ctx := context.Background()

	_, _, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no JetStream client configured")

	require.Error(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "s3://models/x/", "snap-0001"))
	require.Error(t, store.DeleteWeights(ctx, "meta.llama3-2-1b-instruct-v1:0"))

	_, err = store.ListWeights(ctx)
	require.Error(t, err)
}
