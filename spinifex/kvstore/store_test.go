package kvstore_test

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// record is a stand-in for any caller's JSON document.
type record struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// newStore starts an embedded JetStream server and returns a Store over a
// bucket on it. Each test gets its own server, so bucket names need not differ.
func newStore(t *testing.T) *kvstore.Store[record] {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return kvstore.New[record](testutil.NewJetStream(t, nc), kvstore.Config{
		Name:    "kvstore-test",
		History: 1,
		Missing: "kvstore test: no JetStream client configured",
	})
}

// seedRaw writes a record straight to the bucket, bypassing Store, so a test
// can move the revision underneath an in-flight Mutate.
func seedRaw(t *testing.T, store *kvstore.Store[record], key string, rec record) {
	t.Helper()
	kv, err := store.KV(t.Context())
	require.NoError(t, err)
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)
}

func TestStore_GetAbsentKeyIsNotFound(t *testing.T) {
	store := newStore(t)

	_, _, err := store.Get(t.Context(), "nobody")
	require.ErrorIs(t, err, kvstore.ErrNotFound)
	assert.ErrorContains(t, err, "nobody", "the sentinel should name the key it could not find")
}

func TestStore_CreateRoundTripsAndRejectsDuplicate(t *testing.T) {
	store := newStore(t)

	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one", Count: 3}))

	got, rev, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, record{Name: "one", Count: 3}, *got)
	assert.NotZero(t, rev, "a committed record has a revision")

	err = store.Create(t.Context(), "acct-a/one", &record{Name: "impostor"})
	require.ErrorIs(t, err, kvstore.ErrExists)

	// The loser of the create must not have overwritten the winner.
	got, _, err = store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, "one", got.Name)
}

func TestStore_DeleteIsIdempotent(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one"}))

	require.NoError(t, store.Delete(t.Context(), "acct-a/one"))
	require.NoError(t, store.Delete(t.Context(), "acct-a/one"), "deleting an absent key is success")
	require.NoError(t, store.Delete(t.Context(), "never-existed"))

	_, _, err := store.Get(t.Context(), "acct-a/one")
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

func TestStore_ListOnEmptyBucketIsNotAnError(t *testing.T) {
	store := newStore(t)

	out, err := store.List(t.Context(), "")
	require.NoError(t, err, "an empty bucket is an empty listing, not a failure")
	assert.Empty(t, out)
}

func TestStore_ListFiltersByPrefix(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one"}))
	require.NoError(t, store.Create(t.Context(), "acct-a/two", &record{Name: "two"}))
	require.NoError(t, store.Create(t.Context(), "acct-b/three", &record{Name: "three"}))

	mine, err := store.List(t.Context(), "acct-a/")
	require.NoError(t, err)
	assert.Len(t, mine, 2, "one tenant's listing must not surface another's")

	all, err := store.List(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, all, 3, "an empty prefix matches everything")
}

// TestStore_MutateRetriesARevisionConflict forces the conflict deterministically
// rather than racing two goroutines: the first mutate attempt writes a competing
// value out of band, so the commit that follows it must lose and re-read.
func TestStore_MutateRetriesARevisionConflict(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one", Count: 1}))

	attempts := 0
	err := store.Mutate(t.Context(), "acct-a/one", func(rec *record) (bool, error) {
		attempts++
		if attempts == 1 {
			seedRaw(t, store, "acct-a/one", record{Name: "one", Count: 99})
		}
		rec.Count++
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, attempts, "the first commit loses the CAS and the loop re-runs mutate")

	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, 100, got.Count, "the retry must re-read the competing write, not reapply to a stale copy")
}

func TestStore_MutateReportingNoChangeCommitsNoWrite(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one", Count: 1}))
	_, before, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)

	err = store.Mutate(t.Context(), "acct-a/one", func(rec *record) (bool, error) {
		rec.Count = 42
		return false, nil
	})
	require.NoError(t, err)

	got, after, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, 1, got.Count, "a mutate reporting no change must not commit its edits")
	assert.Equal(t, before, after, "no write means no new revision")
}

func TestStore_MutateOnAbsentKeyIsNotFound(t *testing.T) {
	store := newStore(t)

	err := store.Mutate(t.Context(), "nobody", func(*record) (bool, error) {
		t.Error("mutate must not run against an absent key")
		return false, nil
	})
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

func TestStore_DeletePrefixLeavesOtherPrefixes(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one"}))
	require.NoError(t, store.Create(t.Context(), "acct-b/two", &record{Name: "two"}))

	require.NoError(t, store.DeletePrefix(t.Context(), "acct-a/"))

	remaining, err := store.List(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "two", remaining[0].Name)

	require.NoError(t, store.DeletePrefix(t.Context(), "acct-a/"), "purging an already-empty prefix is success")
}

func TestStore_NilJetStreamReportsTheConfiguredMessage(t *testing.T) {
	store := kvstore.New[record](nil, kvstore.Config{
		Name:    "kvstore-test",
		Missing: "kvstore test: no JetStream client configured",
	})

	_, _, err := store.Get(t.Context(), "acct-a/one")
	require.ErrorContains(t, err, "no JetStream client configured")

	err = store.Create(t.Context(), "acct-a/one", &record{Name: "one"})
	require.ErrorContains(t, err, "no JetStream client configured")

	_, err = store.List(t.Context(), "")
	require.ErrorContains(t, err, "no JetStream client configured")
}
