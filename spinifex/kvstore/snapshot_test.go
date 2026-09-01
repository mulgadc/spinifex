package kvstore_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

type snapRecord struct {
	Name string `json:"name"`
}

func snapshotStore(t *testing.T, name string) *kvstore.Store[snapRecord] {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return kvstore.New[snapRecord](testutil.NewJetStream(t, nc), kvstore.Config{Name: name, History: 1})
}

func TestSnapshotReturnsMatchingRecordsWithTheirRevisions(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-records")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	require.NoError(t, store.Set(t.Context(), "i.b", &snapRecord{Name: "b"}))
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 2)

	byKey := map[string]kvstore.Item[snapRecord]{}
	for _, item := range items {
		byKey[item.Key] = item
		require.NotZero(t, item.Revision)
	}
	require.Equal(t, "a", byKey["i.a"].Value.Name)
	require.Equal(t, "b", byKey["i.b"].Value.Name)
	require.NotContains(t, byKey, "node.1")
	require.Equal(t, byKey["i.b"].Revision, highWater)
}

// The high-water mark tracks the reader's position in the stream, not what
// survives in it. A delete is a message, so a snapshot whose newest event is a
// delete still reaches the watermark and can still lower a counter.
func TestSnapshotCountsATombstoneTowardsTheHighWaterMark(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-tombstone")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	require.NoError(t, store.Set(t.Context(), "i.b", &snapRecord{Name: "b"}))
	require.NoError(t, store.Delete(t.Context(), "i.b"))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "i.a", items[0].Key)

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, watermark, highWater)
	require.Greater(t, highWater, items[0].Revision)
}

func TestSnapshotOfNothingIsNotAnError(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-empty")
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Empty(t, items)
	require.Zero(t, highWater)
}

func TestLastSequenceIsTheNewestMessageMatchingTheFilter(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "watermark-newest")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 1)

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, highWater, watermark)

	// A write outside the filter must not move it, which is what makes the
	// mark comparable to a snapshot of the same filter.
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))
	after, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, watermark, after)
}

func TestLastSequenceIsZeroWhenNothingMatches(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "watermark-empty")
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Zero(t, watermark)
}
