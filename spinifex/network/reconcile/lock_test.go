package reconcile

import (
	"context"
	"testing"
	"time"

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

// shrinkLeaderTiming compresses the leader key's lifetime so a test can outlast
// the TTL in a second rather than a minute.
func shrinkLeaderTiming(t *testing.T, ttl, renew time.Duration) {
	t.Helper()
	oldTTL, oldRenew := reconcileLeaderTTL, reconcileLeaderRenew
	reconcileLeaderTTL, reconcileLeaderRenew = ttl, renew
	t.Cleanup(func() { reconcileLeaderTTL, reconcileLeaderRenew = oldTTL, oldRenew })
}

// A pass can outlive the TTL on its own — one stalled gw-lrp DORA runs ~64s
// against a 60s TTL — so the key must be renewed while the pass runs. Without
// this a peer elects itself mid-pass and two reconcilers race the same OVN NB.
func TestAcquireLeader_RenewsKeyBeyondTTL(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkLeaderTiming(t, time.Second, 100*time.Millisecond)

	release, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-1")
	require.True(t, ok)
	defer release()

	// Well past the TTL: an unrenewed key would have expired by now.
	time.Sleep(2500 * time.Millisecond)

	loserRelease, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-2")
	assert.False(t, ok, "leader key expired mid-pass: a peer can now reconcile concurrently")
	assert.Nil(t, loserRelease)
}

// If the key was lost anyway, the stale holder's release must not delete the
// successor's key — an unguarded delete hands the lock to a third node while
// the successor believes it still holds it.
func TestAcquireLeader_ReleaseDoesNotDeleteSuccessorKey(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	release, ok := AcquireLeader(t.Context(), nc, KVBucketVPCDReconcile, "node-1")
	require.True(t, ok)

	// Stand in for a successor that claimed the key after it expired: the write
	// bumps the revision, so node-1's release is now CAS-stale.
	kv, err := js.KeyValue(t.Context(), KVBucketVPCDReconcile)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), reconcileLeaderKey, []byte("node-2"))
	require.NoError(t, err)

	release()

	entry, err := kv.Get(t.Context(), reconcileLeaderKey)
	require.NoError(t, err, "stale release deleted the successor's leader key")
	assert.Equal(t, "node-2", string(entry.Value()), "successor's key must survive a stale release")
}
