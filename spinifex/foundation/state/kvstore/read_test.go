package kvstore_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rewriting rewrites key every millisecond until stop is closed, as a busy
// reconciler updating a record does while somebody else lists the bucket.
func rewriting(t *testing.T, store *kvstore.Store[snapRecord], key string) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := 0; ; n++ {
			select {
			case <-done:
				return
			default:
			}
			_ = store.Set(context.Background(), key, &snapRecord{Name: strconv.Itoa(n)})
			time.Sleep(time.Millisecond)
		}
	})
	return func() { close(done); wg.Wait() }
}

// A read must hold every key that existed before it began and still exists
// after it ends, however often one of them is rewritten while it runs.
func TestEveryReadHoldsAKeyRewrittenWhileItRuns(t *testing.T) {
	store := snapshotStore(t, "read-during-rewrite")
	ctx := t.Context()
	for _, key := range []string{"t.a", "t.b", "t.c"} {
		require.NoError(t, store.Set(ctx, key, &snapRecord{Name: key}))
	}
	stop := rewriting(t, store, "t.b")
	defer stop()

	var listMisses, snapshotMisses atomic.Int32
	for range 300 {
		list, err := store.List(ctx, "t.")
		require.NoError(t, err)
		if len(list) != 3 {
			listMisses.Add(1)
		}
		items, _, err := store.Snapshot(ctx, "t.*")
		require.NoError(t, err)
		if len(items) != 3 {
			snapshotMisses.Add(1)
		}
	}
	assert.Zero(t, listMisses.Load(), "List dropped a key being rewritten")
	assert.Zero(t, snapshotMisses.Load(), "Snapshot dropped a key being rewritten")
}

// The only key, rewritten as the read starts, can leave the consumer with
// nothing pending at creation; the read must still return it.
func TestAReadOfOneKeyBeingRewrittenStillReturnsIt(t *testing.T) {
	store := snapshotStore(t, "read-one-rewritten")
	ctx := t.Context()
	require.NoError(t, store.Set(ctx, "only", &snapRecord{Name: "first"}))
	stop := rewriting(t, store, "only")
	defer stop()

	for range 300 {
		list, err := store.List(ctx, "")
		require.NoError(t, err)
		require.Len(t, list, 1)
	}
}

func TestAKeyDeletedBeforeTheReadIsAbsent(t *testing.T) {
	store := snapshotStore(t, "read-deleted")
	ctx := t.Context()
	require.NoError(t, store.Set(ctx, "t.kept", &snapRecord{Name: "kept"}))
	require.NoError(t, store.Set(ctx, "t.gone", &snapRecord{Name: "gone"}))
	require.NoError(t, store.Delete(ctx, "t.gone"))

	list, err := store.List(ctx, "t.")
	require.NoError(t, err)
	require.Equal(t, []snapRecord{{Name: "kept"}}, list)

	items, _, err := store.Snapshot(ctx, "t.*")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "t.kept", items[0].Key)
}

func TestListKeepsKeyOrderAndTheLatestValue(t *testing.T) {
	store := snapshotStore(t, "read-order")
	ctx := t.Context()
	for _, key := range []string{"t.c", "t.a", "t.b"} {
		require.NoError(t, store.Set(ctx, key, &snapRecord{Name: key + "-1"}))
	}
	require.NoError(t, store.Set(ctx, "t.a", &snapRecord{Name: "t.a-2"}))

	list, err := store.List(ctx, "t.")
	require.NoError(t, err)
	assert.Equal(t, []snapRecord{{Name: "t.a-2"}, {Name: "t.b-1"}, {Name: "t.c-1"}}, list)
}

type otherRecord struct {
	Count int `json:"count"`
}

// A bucket shared by two record types: each List decodes only its own keys,
// so the other type's shape cannot fail it.
func TestListDecodesOnlyTheKeysItMatches(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	b := kvstore.NewBucket(testutil.NewJetStream(t, nc), kvstore.Config{Name: "read-shared", History: 1})
	names := kvstore.On[snapRecord](b)
	counts := kvstore.On[otherRecord](b)
	ctx := t.Context()
	require.NoError(t, names.Set(ctx, "name.a", &snapRecord{Name: "a"}))
	require.NoError(t, counts.Set(ctx, "count.a", &otherRecord{Count: 1}))
	note := "a string, which no struct decodes"
	require.NoError(t, kvstore.On[string](b).Set(ctx, "raw", &note))

	gotNames, err := names.List(ctx, "name.")
	require.NoError(t, err)
	assert.Equal(t, []snapRecord{{Name: "a"}}, gotNames)
	gotCounts, err := counts.List(ctx, "count.")
	require.NoError(t, err)
	assert.Equal(t, []otherRecord{{Count: 1}}, gotCounts)
}

// A key written after DeletePrefix began may survive; every key that was in
// the bucket before it began and not rewritten since is gone.
func TestDeletePrefixRemovesWhatItEnumerated(t *testing.T) {
	store := snapshotStore(t, "read-delete-prefix")
	ctx := t.Context()
	for _, key := range []string{"t.a", "t.b", "t.c", "keep.x"} {
		require.NoError(t, store.Set(ctx, key, &snapRecord{Name: key}))
	}
	stop := rewriting(t, store, "t.b")
	require.NoError(t, store.DeletePrefix(ctx, "t."))
	stop()

	list, err := store.List(ctx, "")
	require.NoError(t, err)
	names := map[string]bool{}
	for _, r := range list {
		names[r.Name] = true
	}
	assert.False(t, names["t.a"])
	assert.False(t, names["t.c"])
	assert.True(t, names["keep.x"])
}

// A read that cannot finish inside its deadline fails; it never hands back
// whatever it had collected.
func TestAReadPastItsDeadlineFailsRatherThanReturningPart(t *testing.T) {
	store := snapshotStore(t, "read-deadline")
	require.NoError(t, store.Set(t.Context(), "t.a", &snapRecord{Name: "a"}))

	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	list, err := store.List(ctx, "t.")
	require.Error(t, err)
	assert.Nil(t, list)
}
