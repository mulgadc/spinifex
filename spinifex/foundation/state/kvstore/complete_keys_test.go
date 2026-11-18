package kvstore_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cutWatcher delivers some keys and then closes without the end-of-snapshot
// marker, as a watcher does when its context ends mid-listing.
type cutWatcher struct{ ch chan jsapi.KeyValueEntry }

func (w cutWatcher) Updates() <-chan jsapi.KeyValueEntry { return w.ch }
func (w cutWatcher) Stop() error                         { return nil }

type keyEntry struct {
	jsapi.KeyValueEntry

	key string
}

func (e keyEntry) Key() string { return e.key }

type cutKV struct {
	jsapi.KeyValue

	w cutWatcher
}

func (kv cutKV) WatchAll(context.Context, ...jsapi.WatchOpt) (jsapi.KeyWatcher, error) {
	return kv.w, nil
}

func TestCompleteKeys_ClosedBeforeTheSnapshotEndIsAnError(t *testing.T) {
	ch := make(chan jsapi.KeyValueEntry, 1)
	ch <- keyEntry{key: "i.a"}
	close(ch)

	keys, err := kvstore.CompleteKeys(t.Context(), cutKV{w: cutWatcher{ch: ch}})

	require.ErrorIs(t, err, kvstore.ErrIncompleteListing)
	assert.Nil(t, keys)
}

func TestCompleteKeys_ContextEndingBeforeTheSnapshotEndIsAnError(t *testing.T) {
	ch := make(chan jsapi.KeyValueEntry, 1)
	ch <- keyEntry{key: "i.a"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := kvstore.CompleteKeys(ctx, cutKV{w: cutWatcher{ch: ch}})

	require.ErrorIs(t, err, kvstore.ErrIncompleteListing)
}

func TestCompleteKeys_ListsLiveKeysOfARealBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jsapi.KeyValueConfig{Bucket: "complete-keys"})
	require.NoError(t, err)
	empty, err := kvstore.CompleteKeys(t.Context(), kv)
	require.NoError(t, err)
	assert.Empty(t, empty)
	for _, k := range []string{"b", "a", "gone"} {
		_, err := kv.PutString(t.Context(), k, "v")
		require.NoError(t, err)
	}
	require.NoError(t, kv.Delete(t.Context(), "gone"))

	keys, err := kvstore.CompleteKeys(t.Context(), kv)

	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, keys)
}

func TestLeaderValue_MissingDeletedAndPurgedKeysAreNotFound(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jsapi.KeyValueConfig{Bucket: "leader-value"})
	require.NoError(t, err)
	for _, k := range []string{"live", "deleted", "purged"} {
		_, err := kv.PutString(t.Context(), k, "v-"+k)
		require.NoError(t, err)
	}
	require.NoError(t, kv.Delete(t.Context(), "deleted"))
	require.NoError(t, kv.Purge(t.Context(), "purged"))

	raw, _, err := kvstore.LeaderValue(t.Context(), js, "leader-value", "live")
	require.NoError(t, err)
	assert.Equal(t, "v-live", string(raw))
	for _, k := range []string{"missing", "deleted", "purged"} {
		_, _, err := kvstore.LeaderValue(t.Context(), js, "leader-value", k)
		assert.ErrorIs(t, err, jsapi.ErrKeyNotFound, k)
	}
}
