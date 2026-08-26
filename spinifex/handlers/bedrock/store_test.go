package handlers_bedrock

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpointKey_Base64EncodesColonBearingModelID(t *testing.T) {
	key := EndpointKey("000000000000", "meta.llama3-2-1b-instruct-v1:0")
	assert.Equal(t, "000000000000/"+"bWV0YS5sbGFtYTMtMi0xYi1pbnN0cnVjdC12MTow", key)
}

func newTestBucket(t *testing.T) *endpointStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return newEndpointStore(js, 1)
}

func TestStore_CreateGetUpdateDelete(t *testing.T) {
	store := newTestBucket(t)
	key := EndpointKey("000000000000", "test-model")

	// Absent key reads as not-found, not an error.
	_, found, err := store.get(t.Context(), key)
	require.NoError(t, err)
	assert.False(t, found)

	rec := EndpointRecord{AccountID: "000000000000", ModelID: "test-model", State: StateStarting, Generation: 1}
	rev, err := store.Create(t.Context(), key, &rec)
	require.NoError(t, err)
	assert.Positive(t, rev)

	// A second create at the same key is rejected: this is the claim
	// primitive Ensure relies on for cross-replica single-flight.
	_, err = store.Create(t.Context(), key, &rec)
	assert.ErrorIs(t, err, kvstore.ErrExists)

	got, gotRev, found, err := store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, StateStarting, got.State)

	got.State = StateReady
	got.Generation = 2
	require.NoError(t, store.CompareAndSet(t.Context(), key, &got, gotRev))

	// The CAS update must fail against the now-stale revision — this is what
	// stops a launch goroutine and a concurrent writer stomping each other.
	err = store.CompareAndSet(t.Context(), key, &got, gotRev)
	assert.ErrorIs(t, err, kvstore.ErrConflict)

	require.NoError(t, store.Purge(t.Context(), key))
	_, found, err = store.get(t.Context(), key)
	require.NoError(t, err)
	assert.False(t, found)

	// Purging an already-absent key is idempotent.
	require.NoError(t, store.Purge(t.Context(), key))
}

func TestListEndpoints_FiltersByAccountPrefix(t *testing.T) {
	store := newTestBucket(t)

	seed := func(accountID, modelID string, state EndpointState) {
		rec := EndpointRecord{AccountID: accountID, ModelID: modelID, State: state, Generation: 1}
		_, err := store.Create(t.Context(), EndpointKey(accountID, modelID), &rec)
		require.NoError(t, err)
	}
	seed("000000000000", "model-a", StateReady)
	seed("000000000000", "model-b", StateStarting)
	seed("111111111111", "model-c", StateReady)

	recs, err := store.list(t.Context(), "000000000000")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	modelIDs := []string{recs[0].ModelID, recs[1].ModelID}
	assert.ElementsMatch(t, []string{"model-a", "model-b"}, modelIDs)

	recs, err = store.list(t.Context(), "111111111111")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "model-c", recs[0].ModelID)

	recs, err = store.list(t.Context(), "222222222222")
	require.NoError(t, err)
	assert.Empty(t, recs)
}

// A bucket that has never held an endpoint is the normal pre-launch state.
// JetStream reports it as ErrNoKeysFound, which must read as empty, not fail.
func TestListEndpoints_EmptyBucketIsNotAnError(t *testing.T) {
	store := newTestBucket(t)

	recs, err := store.list(t.Context(), "000000000000")
	require.NoError(t, err)
	assert.Empty(t, recs)
}

// TestListAllEndpoints_ReturnsAcrossAccounts is ListEndpoints' single-account
// prefix filter's counterpart: ListAllEndpoints must return every account's
// records in one pass, which is what an operator listing needs to see a
// pinned endpoint alongside the shared platform ones.
func TestListAllEndpoints_ReturnsAcrossAccounts(t *testing.T) {
	store := newTestBucket(t)

	seed := func(accountID, modelID string, state EndpointState, pinned bool) {
		rec := EndpointRecord{AccountID: accountID, ModelID: modelID, State: state, Pinned: pinned, Generation: 1}
		_, err := store.Create(t.Context(), EndpointKey(accountID, modelID), &rec)
		require.NoError(t, err)
	}
	seed("000000000000", "model-a", StateReady, false)
	seed("111111111111", "model-c", StateReady, true)

	recs, err := store.listAll(t.Context())
	require.NoError(t, err)
	require.Len(t, recs, 2)

	byModel := map[string]EndpointRecord{}
	for _, rec := range recs {
		byModel[rec.ModelID] = rec
	}
	assert.Equal(t, "000000000000", byModel["model-a"].AccountID)
	assert.False(t, byModel["model-a"].Pinned)
	assert.Equal(t, "111111111111", byModel["model-c"].AccountID)
	assert.True(t, byModel["model-c"].Pinned)
}

// TestListAllEndpoints_EmptyBucketIsNotAnError mirrors ListEndpoints' own
// empty-bucket guard.
func TestListAllEndpoints_EmptyBucketIsNotAnError(t *testing.T) {
	store := newTestBucket(t)

	recs, err := store.listAll(t.Context())
	require.NoError(t, err)
	assert.Empty(t, recs)
}
