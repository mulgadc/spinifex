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
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "snap-0001"))

	snapshotID, ok, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "snap-0001", snapshotID)
}

// TestWeightsStore_PutOverwritesPrevious covers a re-stage: PutWeights
// overwrites the prior snapshot ID in place rather than erroring or
// accumulating history.
func TestWeightsStore_PutOverwritesPrevious(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewWeightsStore(testutil.NewJetStream(t, nc), 1)

	ctx := context.Background()
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "snap-0001"))
	require.NoError(t, store.PutWeights(ctx, "meta.llama3-2-1b-instruct-v1:0", "snap-0002"))

	snapshotID, ok, err := store.Resolve(ctx, "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "snap-0002", snapshotID)
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
