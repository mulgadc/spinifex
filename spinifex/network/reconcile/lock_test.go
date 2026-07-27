package reconcile

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquireLeader_GetOrCreateAcrossCalls pins the get-or-create fix: first
// acquire creates the bucket and wins; second must attach (not hang on "stream
// name already in use") and lose; after release a new acquire wins.
func TestAcquireLeader_GetOrCreateAcrossCalls(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	release, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-1")
	require.True(t, ok, "first acquire must win and create the bucket")
	require.NotNil(t, release)

	// Bucket now exists; the second acquire must reach the Create-key contention
	// path (and lose) rather than dead-ending on CreateKeyValue.
	loserRelease, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-2")
	assert.False(t, ok, "second acquire must lose while the lock is held")
	assert.Nil(t, loserRelease)

	// Releasing frees the key so a subsequent acquire can take over.
	release()
	release2, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-3")
	require.True(t, ok, "acquire after release must win")
	require.NotNil(t, release2)
	release2()
}

// TestAcquireLeader_ReleaseSurvivesCancelledContext pins that the release runs
// on a context of its own. Shutdown cancels the acquiring context first and then
// releases, so a release bound to that context would silently no-op and park the
// lock for the full TTL, blocking every other node's reconcile.
func TestAcquireLeader_ReleaseSurvivesCancelledContext(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	ctx, cancel := context.WithCancel(t.Context())
	release, ok := AcquireLeader(ctx, nc, KVBucketVPCDReconcile, "node-1")
	require.True(t, ok)

	cancel()
	release()

	release2, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-2")
	require.True(t, ok, "lock must be free immediately after release, not held until the TTL reaps it")
	release2()
}
