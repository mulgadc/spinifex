package kvstore_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recoveryBucket = "kvstore-recovery-test"

// newRecoverableStore returns a store whose bucket may be recreated by recovery,
// already open so the next operation is the one that meets the lost stream.
func newRecoverableStore(t *testing.T, cfg kvstore.Config) (*server.Server, jetstream.JetStream, *kvstore.Store[record]) {
	t.Helper()
	ns, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	cfg.Name = recoveryBucket
	cfg.History = 1
	store := kvstore.New[record](js, cfg)
	_, err := store.KV(t.Context())
	require.NoError(t, err, "the bucket must be open before the stream is taken away")
	return ns, js, store
}

// loseStream deletes the bucket out from under the store's memoised handle,
// which is what cluster formation does to a low-replication stream.
func loseStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	require.NoError(t, js.DeleteKeyValue(t.Context(), recoveryBucket))
}

// TestStore_RecoversAfterStreamLost runs every operation against a bucket whose
// stream has gone, and asserts each one reaches the recreated bucket. The
// recreated bucket is empty, so a read reporting ErrNotFound has recovered:
// what must not survive is a stream-unavailable error reaching the caller.
func TestStore_RecoversAfterStreamLost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		op      func(context.Context, *kvstore.Store[record]) error
		wantErr error
	}{
		{"Get", func(ctx context.Context, s *kvstore.Store[record]) error {
			_, _, err := s.Get(ctx, "acct-a/one")
			return err
		}, kvstore.ErrNotFound},
		{"Create", func(ctx context.Context, s *kvstore.Store[record]) error {
			_, err := s.Create(ctx, "acct-a/one", &record{Name: "one"})
			return err
		}, nil},
		{"Set", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.Set(ctx, "acct-a/one", &record{Name: "one"})
		}, nil},
		{"Mutate", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.Mutate(ctx, "acct-a/one", func(*record) (bool, error) { return true, nil })
		}, kvstore.ErrNotFound},
		{"Upsert", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.Upsert(ctx, "counter", func(r *record) (bool, error) {
				r.Count++
				return true, nil
			})
		}, nil},
		{"Delete", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.Delete(ctx, "acct-a/one")
		}, nil},
		{"Purge", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.Purge(ctx, "acct-a/one")
		}, nil},
		{"Exists", func(ctx context.Context, s *kvstore.Store[record]) error {
			_, err := s.Exists(ctx, "acct-a/one")
			return err
		}, nil},
		{"List", func(ctx context.Context, s *kvstore.Store[record]) error {
			_, err := s.List(ctx, "")
			return err
		}, nil},
		{"DeletePrefix", func(ctx context.Context, s *kvstore.Store[record]) error {
			return s.DeletePrefix(ctx, "acct-a/")
		}, nil},
		{"Watch", func(ctx context.Context, s *kvstore.Store[record]) error {
			w, err := s.Watch(ctx, ">")
			if err == nil {
				_ = w.Stop()
			}
			return err
		}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, js, store := newRecoverableStore(t, kvstore.Config{RecreateIfMissing: true})
			require.NoError(t, store.Set(t.Context(), "acct-a/seed", &record{Name: "seed"}))
			loseStream(t, js)

			err := tt.op(t.Context(), store)
			assert.False(t, kvutil.IsStreamUnavailable(err),
				"the operation must reach the reopened bucket, not surface the loss: %v", err)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			// The recreated bucket is the one the store now holds, so a
			// subsequent write lands without a second recovery.
			require.NoError(t, store.Set(t.Context(), "acct-a/after", &record{Name: "after"}))
			got, _, err := store.Get(t.Context(), "acct-a/after")
			require.NoError(t, err)
			assert.Equal(t, "after", got.Name)
		})
	}
}

// TestStore_RecreateIfMissingDefaultsOff is the guard on the data-loss half of
// recovery: a bucket that existed and now does not has lost its records, and a
// store that has not opted in must say so rather than hand back an empty one.
func TestStore_RecreateIfMissingDefaultsOff(t *testing.T) {
	t.Parallel()
	_, js, store := newRecoverableStore(t, kvstore.Config{})
	require.NoError(t, store.Set(t.Context(), "acct-a/one", &record{Name: "one"}))
	loseStream(t, js)

	err := store.Set(t.Context(), "acct-a/one", &record{Name: "two"})
	require.Error(t, err, "recovery must not silently recreate a bucket the caller did not opt into")

	_, err = js.KeyValue(t.Context(), recoveryBucket)
	require.Error(t, err, "the bucket must still be absent")
}

// TestStore_ReconnectsWithoutRecreating covers the first half of Reopen: a
// bucket that is merely unreachable through a stale handle is reconnected, and
// its records are still there. RecreateIfMissing is off, so nothing else could
// have produced this result.
func TestStore_ReconnectsWithoutRecreating(t *testing.T) {
	t.Parallel()
	_, js, store := newRecoverableStore(t, kvstore.Config{})
	require.NoError(t, store.Set(t.Context(), "acct-a/one", &record{Name: "one"}))

	kv, err := store.Reopen(t.Context())
	require.NoError(t, err)
	require.NotNil(t, kv)

	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, "one", got.Name, "a reconnect keeps the records a recreate would have dropped")

	_, err = js.KeyValue(t.Context(), recoveryBucket)
	require.NoError(t, err)
}

// TestStore_RecoveryFailureSurfacesTheOriginalError pins which of the two errors
// the caller sees. With the server gone both the operation and the reopen fail,
// and the operation's error is the one describing what the caller was doing.
func TestStore_RecoveryFailureSurfacesTheOriginalError(t *testing.T) {
	t.Parallel()
	ns, _, store := newRecoverableStore(t, kvstore.Config{RecreateIfMissing: true})
	ns.Shutdown()
	ns.WaitForShutdown()

	// The server is gone, so this Set can only time out. Bound it: a
	// deadline-free context leaves the jetstream client waiting out its own
	// five-second default for a result the shutdown above already decided.
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	err := store.Set(ctx, "acct-a/one", &record{Name: "one"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "put acct-a/one",
		"the surfaced error must name the operation, not the failed reopen")
}

// TestStore_CompareAndSetDoesNotReRun is the documented exception to the retry.
// The revision is the caller's, so a second attempt against a reopened bucket
// would be committing a precondition that no longer means anything.
func TestStore_CompareAndSetDoesNotReRun(t *testing.T) {
	t.Parallel()
	_, js, store := newRecoverableStore(t, kvstore.Config{RecreateIfMissing: true})
	rev, err := store.Create(t.Context(), "acct-a/one", &record{Name: "one"})
	require.NoError(t, err)
	loseStream(t, js)

	err = store.CompareAndSet(t.Context(), "acct-a/one", &record{Name: "two"}, rev)
	require.Error(t, err, "a revision-guarded write must not be replayed onto a reopened bucket")
	assert.ErrorContains(t, err, "acct-a/one")
}

// TestBucket_OnOpenRunsOnEveryOpen covers the hook a recreated bucket depends
// on: an unstamped, unmigrated bucket is not a recovered one.
func TestBucket_OnOpenRunsOnEveryOpen(t *testing.T) {
	t.Parallel()
	var opens atomic.Int32
	_, js, store := newRecoverableStore(t, kvstore.Config{
		RecreateIfMissing: true,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			opens.Add(1)
			return kvutil.WriteVersion(ctx, kv, 7)
		},
	})

	// newRecoverableStore opened it once already; the count is what the reopen
	// adds to that.
	before := opens.Load()
	require.Positive(t, before, "the hook must run on the first open")

	loseStream(t, js)
	require.NoError(t, store.Set(t.Context(), "acct-a/one", &record{Name: "one"}))
	assert.Equal(t, before+1, opens.Load(), "the hook must run again on a recovery reopen")

	kv, err := store.KV(t.Context())
	require.NoError(t, err)
	version, err := kvutil.ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, 7, version, "a recreated bucket must be re-stamped, not left unversioned")
}

// TestBucket_OnOpenFailureFailsTheOpen keeps a half-opened bucket from being
// memoised: a migration that could not run leaves records the caller's decoder
// will not understand, so the open is the right place to stop.
func TestBucket_OnOpenFailureFailsTheOpen(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	boom := errors.New("migration refused")
	store := kvstore.New[record](testutil.NewJetStream(t, nc), kvstore.Config{
		Name:    "kvstore-onopen-fail-test",
		History: 1,
		OnOpen: func(context.Context, jetstream.KeyValue) error {
			return boom
		},
	})

	_, _, err := store.Get(t.Context(), "acct-a/one")
	require.ErrorIs(t, err, boom)

	_, _, err = store.Get(t.Context(), "acct-a/one")
	require.ErrorIs(t, err, boom, "a failed open must not be memoised as a good one")
}

// TestBucket_ReopenWithoutAJetStreamClientReportsTheConfiguredMessage covers the
// eager caller that passed nil: it cannot recover, and must say why.
func TestBucket_ReopenWithoutAJetStreamClientReportsTheConfiguredMessage(t *testing.T) {
	t.Parallel()
	bucket := kvstore.NewOpenBucket(nil, newOpenBucket(t), kvstore.Config{
		Name:    "kvstore-over-test",
		Missing: "kvstore test: no JetStream client configured",
	})

	_, err := bucket.Reopen(t.Context())
	require.ErrorContains(t, err, "no JetStream client configured")
}

// TestOn_TwoTypedViewsShareOneBucket covers the shape a bucket holding more
// than one record type needs: each view decodes its own records, and they open
// the bucket once between them rather than once each.
func TestOn_TwoTypedViewsShareOneBucket(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	var opens atomic.Int32
	bucket := kvstore.NewBucket(testutil.NewJetStream(t, nc), kvstore.Config{
		Name:    "kvstore-views-test",
		History: 1,
		OnOpen: func(context.Context, jetstream.KeyValue) error {
			opens.Add(1)
			return nil
		},
	})
	records := kvstore.On[record](bucket)
	counters := kvstore.On[counter](bucket)

	require.NoError(t, records.Set(t.Context(), "rec.one", &record{Name: "one"}))
	require.NoError(t, counters.Set(t.Context(), "cnt.one", &counter{Total: 5}))
	assert.Equal(t, int32(1), opens.Load(), "both views must share the one open")

	got, _, err := records.Get(t.Context(), "rec.one")
	require.NoError(t, err)
	assert.Equal(t, "one", got.Name)

	tally, _, err := counters.Get(t.Context(), "cnt.one")
	require.NoError(t, err)
	assert.Equal(t, 5, tally.Total)

	// Each view lists only its own prefix, so neither decodes the other's.
	mine, err := records.List(t.Context(), "rec.")
	require.NoError(t, err)
	assert.Len(t, mine, 1)
}

// counter is a second record type over the same bucket as record.
type counter struct {
	Total int `json:"total"`
}

// TestOn_RecoveryThroughOneViewRepairsTheOther is the reason the views share a
// Bucket rather than holding a handle each: a stale handle repaired by one
// view must not leave the other still pointing at the lost stream.
func TestOn_RecoveryThroughOneViewRepairsTheOther(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	bucket := kvstore.NewBucket(js, kvstore.Config{
		Name:              "kvstore-views-recovery-test",
		History:           1,
		RecreateIfMissing: true,
	})
	records := kvstore.On[record](bucket)
	counters := kvstore.On[counter](bucket)
	require.NoError(t, records.Set(t.Context(), "rec.one", &record{Name: "one"}))

	require.NoError(t, js.DeleteKeyValue(t.Context(), "kvstore-views-recovery-test"))

	// Recover through one view.
	require.NoError(t, records.Set(t.Context(), "rec.two", &record{Name: "two"}))
	// The other must already be on the repaired handle.
	require.NoError(t, counters.Set(t.Context(), "cnt.one", &counter{Total: 1}))

	tally, _, err := counters.Get(t.Context(), "cnt.one")
	require.NoError(t, err)
	assert.Equal(t, 1, tally.Total)
}

// TestStore_ReplaceRetriesARevisionConflict is Replace's difference from Set:
// a write that loses a race is retried against the winner's revision instead of
// landing out of order.
func TestStore_ReplaceRetriesARevisionConflict(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	mustCreate(t, store, "acct-a/one", record{Name: "one", Count: 1})

	require.NoError(t, store.Replace(t.Context(), "acct-a/one", &record{Name: "replaced", Count: 9}))
	got, _, err := store.Get(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.Equal(t, record{Name: "replaced", Count: 9}, *got, "Replace overwrites wholesale")

	require.NoError(t, store.Replace(t.Context(), "acct-a/new", &record{Name: "new"}))
	got, _, err = store.Get(t.Context(), "acct-a/new")
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name, "Replace on an absent key creates it")
}

// TestStore_ClaimIsWonOnce pins the delete-as-claim: the first caller gets the
// record, and every later one is told there was nothing to take rather than
// handed a second copy.
func TestStore_ClaimIsWonOnce(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	mustCreate(t, store, "acct-a/one", record{Name: "one", Count: 3})

	got, notFound, err := store.Claim(t.Context(), "acct-a/one")
	require.NoError(t, err)
	require.False(t, notFound)
	assert.Equal(t, record{Name: "one", Count: 3}, *got)

	_, notFound, err = store.Claim(t.Context(), "acct-a/one")
	require.NoError(t, err, "a lost claim is an answer, not an error")
	assert.True(t, notFound)

	present, err := store.Exists(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.False(t, present, "the claim takes the record with it")
}

// TestStore_ClaimRecoversAfterStreamLost keeps Claim on the recovery path. The
// recreated bucket holds nothing, so notFound is the correct answer — what must
// not happen is the stream error reaching the caller.
func TestStore_ClaimRecoversAfterStreamLost(t *testing.T) {
	t.Parallel()
	_, js, store := newRecoverableStore(t, kvstore.Config{RecreateIfMissing: true})
	require.NoError(t, store.Set(t.Context(), "acct-a/one", &record{Name: "one"}))
	loseStream(t, js)

	_, notFound, err := store.Claim(t.Context(), "acct-a/one")
	require.NoError(t, err)
	assert.True(t, notFound)
}

// TestBucket_DescriptionReachesTheCreatedBucket covers Config.Description,
// which exists so a bucket recreated by recovery is not left anonymous.
func TestBucket_DescriptionReachesTheCreatedBucket(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	bucket := kvstore.NewBucket(js, kvstore.Config{
		Name:              "kvstore-description-test",
		Description:       "a bucket that says what it is",
		History:           1,
		RecreateIfMissing: true,
	})
	kv, err := bucket.KV(t.Context())
	require.NoError(t, err)

	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "a bucket that says what it is", status.(*jetstream.KeyValueBucketStatus).StreamInfo().Config.Description)
}
