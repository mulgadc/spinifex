package kvstore_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
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

// TestStore_NilJetStreamWithNoMessageStillSaysSomething guards the fallback: a
// Config that omits Missing must not produce an error with an empty string,
// which reads as a passing call to anything logging err.Error().
func TestStore_NilJetStreamWithNoMessageStillSaysSomething(t *testing.T) {
	store := kvstore.New[record](nil, kvstore.Config{Name: "kvstore-test"})

	_, _, err := store.Get(t.Context(), "acct-a/one")
	require.Error(t, err)
	assert.NotEmpty(t, err.Error(), "an error with no message is worse than a wrong one")
	assert.ErrorContains(t, err, "no JetStream client configured")
}

// newOpenBucket starts an embedded JetStream server and returns an already-open
// bucket handle, the shape an eager caller holds at construction.
func newOpenBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := kvutil.GetOrCreateBucket(t.Context(), testutil.NewJetStream(t, nc), "kvstore-over-test", 1)
	require.NoError(t, err)
	return kv
}

// TestOver_ServesAPreOpenedBucket covers the eager-caller path end to end. It
// also pins the order of the checks in Bucket.KV: a Store built by Over holds
// no JetStream client, so a nil-client guard running before the memo check
// would fail every one of these calls with an empty error string.
func TestOver_ServesAPreOpenedBucket(t *testing.T) {
	kv := newOpenBucket(t)

	// A record written straight to the bucket, as a pre-existing one would be.
	data, err := json.Marshal(record{Name: "one", Count: 7})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "acct-a/one", data)
	require.NoError(t, err)

	store := kvstore.Over[record](kv, kvstore.Config{})

	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, record{Name: "one", Count: 7}, *got)

	require.NoError(t, store.Set(t.Context(), "acct-a/two", &record{Name: "two"}))
	all, err := store.List(t.Context(), "acct-a/")
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestStore_SetOverwritesWhereCreateWouldConflict(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "first"}))
	require.ErrorIs(t, store.Create(t.Context(), "acct-a/one", &record{Name: "second"}), kvstore.ErrExists)

	require.NoError(t, store.Set(t.Context(), "acct-a/one", &record{Name: "second", Count: 9}))
	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, record{Name: "second", Count: 9}, *got, "Set replaces where Create refuses")

	require.NoError(t, store.Set(t.Context(), "acct-a/new", &record{Name: "new"}))
	got, _, err = store.Get(t.Context(), "acct-a/new")
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name, "Set on an absent key creates it")
}

// TestStore_ExistsDoesNotDecode is the reason Exists is a method rather than a
// Get in disguise: a record whose bytes will not unmarshal must still be
// reported as present, or it becomes impossible to delete.
func TestStore_ExistsDoesNotDecode(t *testing.T) {
	store := newStore(t)
	kv, err := store.KV(t.Context())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "acct-a/corrupt", []byte("{not json"))
	require.NoError(t, err)

	_, _, err = store.Get(t.Context(), "acct-a/corrupt")
	require.Error(t, err, "Get cannot read it")

	present, err := store.Exists(t.Context(), "acct-a/corrupt")
	require.NoError(t, err)
	assert.True(t, present, "a corrupt record is still present, and must stay deletable")

	require.NoError(t, store.Delete(t.Context(), "acct-a/corrupt"))
	present, err = store.Exists(t.Context(), "acct-a/corrupt")
	require.NoError(t, err)
	assert.False(t, present)
}

func TestStore_ExistsIsFalseForAnAbsentKey(t *testing.T) {
	store := newStore(t)

	present, err := store.Exists(t.Context(), "nobody")
	require.NoError(t, err, "an absent key is not an error, unlike Get")
	assert.False(t, present)
}

func TestStore_CompareAndSetRejectsAStaleRevision(t *testing.T) {
	store := newStore(t)
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one", Count: 1}))
	_, stale, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)

	// Another writer commits first, moving the revision on.
	seedRaw(t, store, "acct-a/one", record{Name: "one", Count: 2})

	err = store.CompareAndSet(t.Context(), "acct-a/one", &record{Name: "one", Count: 99}, stale)
	require.ErrorIs(t, err, kvstore.ErrConflict)

	got, fresh, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, 2, got.Count, "a rejected write must leave the winner's value in place")

	require.NoError(t, store.CompareAndSet(t.Context(), "acct-a/one", &record{Name: "one", Count: 3}, fresh))
	got, _, err = store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, 3, got.Count, "the same call commits at the current revision")
}

// TestStore_ConfigAttemptsBoundsMutate pins both halves of the per-store budget:
// the loop stops after Attempts tries, and the caller's Exhausted error is what
// surfaces rather than kvutil's default message.
func TestStore_ConfigAttemptsBoundsMutate(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	errBudgetSpent := errors.New("budget spent")
	var gotKey string
	var gotAttempts int

	store := kvstore.New[record](testutil.NewJetStream(t, nc), kvstore.Config{
		Name:     "kvstore-attempts-test",
		History:  1,
		Attempts: 2,
		Exhausted: func(key string, attempts int) error {
			gotKey, gotAttempts = key, attempts
			return errBudgetSpent
		},
	})
	require.NoError(t, store.Create(t.Context(), "acct-a/one", &record{Name: "one"}))

	calls := 0
	err := store.Mutate(t.Context(), "acct-a/one", func(rec *record) (bool, error) {
		calls++
		// Move the revision on every attempt, so no commit can ever win.
		seedRaw(t, store, "acct-a/one", record{Name: "one", Count: calls})
		rec.Count = 100
		return true, nil
	})
	require.ErrorIs(t, err, errBudgetSpent, "the configured error must surface, not kvutil's default")
	assert.Equal(t, 2, calls, "Attempts bounds the loop")
	assert.Equal(t, "acct-a/one", gotKey)
	assert.Equal(t, 2, gotAttempts, "Exhausted is told the budget it spent")

	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, 2, got.Count, "an exhausted Mutate commits nothing of its own")
}
