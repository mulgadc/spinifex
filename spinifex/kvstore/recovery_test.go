package kvstore_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
	ns, _, store := newRecoverableStore(t, kvstore.Config{RecreateIfMissing: true})
	ns.Shutdown()
	ns.WaitForShutdown()

	err := store.Set(t.Context(), "acct-a/one", &record{Name: "one"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "put acct-a/one",
		"the surfaced error must name the operation, not the failed reopen")
}

// TestStore_CompareAndSetDoesNotReRun is the documented exception to the retry.
// The revision is the caller's, so a second attempt against a reopened bucket
// would be committing a precondition that no longer means anything.
func TestStore_CompareAndSetDoesNotReRun(t *testing.T) {
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
	bucket := kvstore.NewOpenBucket(nil, newOpenBucket(t), kvstore.Config{
		Name:    "kvstore-over-test",
		Missing: "kvstore test: no JetStream client configured",
	})

	_, err := bucket.Reopen(t.Context())
	require.ErrorContains(t, err, "no JetStream client configured")
}
