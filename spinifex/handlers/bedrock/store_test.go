package handlers_bedrock

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpointKey_Base64EncodesColonBearingModelID(t *testing.T) {
	key := EndpointKey("000000000000", "meta.llama3-2-1b-instruct-v1:0")
	assert.Equal(t, "000000000000/"+"bWV0YS5sbGFtYTMtMi0xYi1pbnN0cnVjdC12MTow", key)
}

func newTestBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateEndpointsBucket(t.Context(), js, 1)
	require.NoError(t, err)
	return kv
}

func TestStore_CreateGetUpdateDelete(t *testing.T) {
	kv := newTestBucket(t)
	key := EndpointKey("000000000000", "test-model")

	// Absent key reads as not-found, not an error.
	var rec EndpointRecord
	found, err := getJSON(t.Context(), kv, key, &rec)
	require.NoError(t, err)
	assert.False(t, found)

	rec = EndpointRecord{AccountID: "000000000000", ModelID: "test-model", State: StateStarting, Generation: 1}
	rev, err := createJSONRevision(t.Context(), kv, key, rec)
	require.NoError(t, err)
	assert.Positive(t, rev)

	// A second create at the same key is rejected: this is the claim
	// primitive Ensure relies on for cross-replica single-flight.
	_, err = createJSONRevision(t.Context(), kv, key, rec)
	assert.ErrorIs(t, err, jetstream.ErrKeyExists)

	var got EndpointRecord
	gotRev, found, err := getJSONRevision(t.Context(), kv, key, &got)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, StateStarting, got.State)

	got.State = StateReady
	got.Generation = 2
	require.NoError(t, updateJSON(t.Context(), kv, key, gotRev, got))

	// The CAS update must fail against the now-stale revision — this is what
	// stops a launch goroutine and a concurrent writer stomping each other.
	err = updateJSON(t.Context(), kv, key, gotRev, got)
	assert.Error(t, err)

	require.NoError(t, deleteJSON(t.Context(), kv, key))
	found, err = getJSON(t.Context(), kv, key, &rec)
	require.NoError(t, err)
	assert.False(t, found)

	// Deleting an already-absent key is idempotent.
	require.NoError(t, deleteJSON(t.Context(), kv, key))
}

func TestListEndpoints_FiltersByAccountPrefix(t *testing.T) {
	kv := newTestBucket(t)

	seed := func(accountID, modelID string, state EndpointState) {
		rec := EndpointRecord{AccountID: accountID, ModelID: modelID, State: state, Generation: 1}
		_, err := createJSONRevision(t.Context(), kv, EndpointKey(accountID, modelID), rec)
		require.NoError(t, err)
	}
	seed("000000000000", "model-a", StateReady)
	seed("000000000000", "model-b", StateStarting)
	seed("111111111111", "model-c", StateReady)

	recs, err := ListEndpoints(t.Context(), kv, "000000000000")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	modelIDs := []string{recs[0].ModelID, recs[1].ModelID}
	assert.ElementsMatch(t, []string{"model-a", "model-b"}, modelIDs)

	recs, err = ListEndpoints(t.Context(), kv, "111111111111")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "model-c", recs[0].ModelID)

	recs, err = ListEndpoints(t.Context(), kv, "222222222222")
	require.NoError(t, err)
	assert.Empty(t, recs)
}
