// Shows what the current endpoint readers do with a marker key in the
// endpoints bucket, through the unexported store they list with.
//
//test:in-package — drives the unexported endpointStore list paths.
package ochre

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const compatMarkerKey = "_readiness.marker.3f9c1d7e"

// endpointsWithRecord opens the endpoints bucket the way the service does and
// stores one real record, returning the raw bucket for writing markers.
func endpointsWithRecord(t *testing.T) (*endpointStore, jetstream.JetStream, jetstream.KeyValue, string) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store := newEndpointStore(js)
	key := EndpointKey("000000000000", "test-model")
	_, err := store.Create(t.Context(), key, &EndpointRecord{AccountID: "000000000000", ModelID: "test-model", State: StateReady})
	require.NoError(t, err)
	kv, err := js.KeyValue(t.Context(), KVBucketEndpoints)
	require.NoError(t, err)
	return store, js, kv, key
}

func TestMarkerCompat_ValueMarkerBreaksTheOperatorListing(t *testing.T) {
	t.Run("json value is a phantom endpoint", func(t *testing.T) {
		store, _, kv, _ := endpointsWithRecord(t)
		_, err := kv.Create(t.Context(), compatMarkerKey, []byte(`{"nonce":"3f9c1d7e"}`))
		require.NoError(t, err)

		all, err := store.listAll(t.Context())
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Contains(t, all, EndpointRecord{}, "the marker decodes as an empty endpoint record")
		items, _, err := store.Snapshot(t.Context(), ">")
		require.NoError(t, err)
		assert.Len(t, items, 2)

		platform, err := store.list(t.Context(), "000000000000")
		require.NoError(t, err)
		assert.Len(t, platform, 1, "an account-prefixed listing does not see the marker")
	})
	t.Run("non-json value fails the listing", func(t *testing.T) {
		store, _, kv, _ := endpointsWithRecord(t)
		_, err := kv.Create(t.Context(), compatMarkerKey, []byte("3f9c1d7e"))
		require.NoError(t, err)

		_, err = store.listAll(t.Context())
		require.ErrorContains(t, err, "decode "+compatMarkerKey)
		_, _, err = store.Snapshot(t.Context(), ">")
		require.ErrorContains(t, err, "decode "+compatMarkerKey)
	})
}

func TestMarkerCompat_DeleteMarkerIsInvisibleToCurrentReaders(t *testing.T) {
	store, js, kv, key := endpointsWithRecord(t)
	require.NoError(t, kv.Delete(t.Context(), compatMarkerKey))
	b, err := readiness.Observe(t.Context(), js, KVBucketEndpoints, compatMarkerKey)
	require.NoError(t, err)
	s, _, err := readiness.Attach(t.Context(), js, b)
	require.NoError(t, err)

	// Current readers and writers keep working beside the marker, including a
	// record update and delete as a rolled-back node would make.
	require.NoError(t, store.Mutate(t.Context(), key, func(r *EndpointRecord) (bool, error) {
		r.State = StateDraining
		return true, nil
	}))
	all, err := store.listAll(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, StateDraining, all[0].State)
	items, highWater, err := store.Snapshot(t.Context(), ">")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.GreaterOrEqual(t, highWater, b.Marker.Seq, "the delete marker still counts towards the snapshot mark")
	_, found, err := store.get(t.Context(), compatMarkerKey)
	require.NoError(t, err)
	assert.False(t, found)
	require.NoError(t, kv.Delete(t.Context(), key))
	all, err = store.listAll(t.Context())
	require.NoError(t, err)
	assert.Empty(t, all)
	require.NoError(t, s.Verify(t.Context()), "ordinary endpoint traffic leaves the marker in place")

	raw, err := kvstore.LeaderMsgGet(t.Context(), js, "KV_"+KVBucketEndpoints,
		kvstore.MsgGetRequest{LastFor: "$KV." + KVBucketEndpoints + "." + compatMarkerKey})
	require.NoError(t, err)
	assert.Equal(t, "DEL", raw.Header.Get("KV-Operation"))
	assert.Empty(t, raw.Data)
}

func TestMarkerCompat_DeleteMarkerLossFailsClosed(t *testing.T) {
	t.Run("delete markers purged", func(t *testing.T) {
		_, js, kv, _ := endpointsWithRecord(t)
		require.NoError(t, kv.Delete(t.Context(), compatMarkerKey))
		b, err := readiness.Observe(t.Context(), js, KVBucketEndpoints, compatMarkerKey)
		require.NoError(t, err)
		s, _, err := readiness.Attach(t.Context(), js, b)
		require.NoError(t, err)

		require.NoError(t, kv.PurgeDeletes(t.Context(), jetstream.DeleteMarkersOlderThan(-1)))
		require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamMissing)
		_, _, err = readiness.Attach(t.Context(), js, b)
		require.ErrorIs(t, err, readiness.ErrStreamMissing)
	})
	t.Run("bucket recreated by a restarted current reader", func(t *testing.T) {
		_, js, kv, _ := endpointsWithRecord(t)
		require.NoError(t, kv.Delete(t.Context(), compatMarkerKey))
		b, err := readiness.Observe(t.Context(), js, KVBucketEndpoints, compatMarkerKey)
		require.NoError(t, err)

		require.NoError(t, js.DeleteKeyValue(t.Context(), KVBucketEndpoints))
		all, err := newEndpointStore(js).listAll(t.Context())
		require.NoError(t, err, "a fresh store recreates a lost bucket")
		assert.Empty(t, all)
		_, _, err = readiness.Attach(t.Context(), js, b)
		require.ErrorIs(t, err, readiness.ErrStreamReplaced)
	})
}
