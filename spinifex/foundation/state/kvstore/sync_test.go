package kvstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncBucket_OpensWithOneRevisionNoTTLAndASchemaVersion(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.OpenSyncBucket(js)
	kv, err := b.KV(t.Context())
	require.NoError(t, err)

	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "spinifex-sync", status.Bucket())
	assert.Equal(t, int64(1), status.History())
	assert.Zero(t, status.TTL(), "a lease's expiry is observed by contenders, never kept by the store")
	info := status.(*jetstream.KeyValueBucketStatus).StreamInfo()
	assert.Equal(t, jetstream.FileStorage, info.Config.Storage)

	version, err := kvstore.ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, 1, version)
}

func TestSyncBucket_ErrorsCarryBothPackagesSentinels(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.OpenSyncBucket(js)

	_, _, err := b.Load(t.Context(), "k")
	assert.ErrorIs(t, err, kvsync.ErrNotFound)
	assert.ErrorIs(t, err, kvstore.ErrNotFound)

	rev, err := b.CompareAndSwap(t.Context(), "k", []byte("a"), 0)
	require.NoError(t, err)

	mismatches := map[string]func() error{
		"create over a live key": func() error {
			_, err := b.CompareAndSwap(t.Context(), "k", []byte("b"), 0)
			return err
		},
		"update at a stale revision": func() error {
			_, err := b.CompareAndSwap(t.Context(), "k", []byte("b"), rev+100)
			return err
		},
		"delete at a stale revision": func() error {
			return b.CompareAndDelete(t.Context(), "k", rev+100)
		},
	}
	for name, op := range mismatches {
		t.Run(name, func(t *testing.T) {
			err := op()
			assert.ErrorIs(t, err, kvsync.ErrRevisionMismatch)
			assert.ErrorIs(t, err, kvstore.ErrConflict)
		})
	}

	value, got, err := b.Load(t.Context(), "k")
	require.NoError(t, err)
	assert.Equal(t, rev, got)
	assert.Equal(t, []byte("a"), value)
}

func TestSyncBucket_RevisionGrowsAcrossADelete(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.OpenSyncBucket(js)

	first, err := b.CompareAndSwap(t.Context(), "k", []byte("a"), 0)
	require.NoError(t, err)
	require.NoError(t, b.CompareAndDelete(t.Context(), "k", first))

	// Rev 0 creates over the delete marker, at a revision above the first.
	second, err := b.CompareAndSwap(t.Context(), "k", []byte("b"), 0)
	require.NoError(t, err)
	assert.Greater(t, second, first)

	third, err := b.CompareAndSwap(t.Context(), "k", []byte("c"), second)
	require.NoError(t, err)
	assert.Greater(t, third, second)
}

func TestSyncBucket_StoppedJetStreamIsUnavailable(t *testing.T) {
	stops := map[string]func(t *testing.T, ns *server.Server){
		"jetstream disabled": func(t *testing.T, ns *server.Server) { require.NoError(t, ns.DisableJetStream()) },
		"server shut down":   func(_ *testing.T, ns *server.Server) { ns.Shutdown(); ns.WaitForShutdown() },
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			ns, _, js := testutil.StartTestJetStream(t)
			b := kvstore.OpenSyncBucket(js)
			rev, err := b.CompareAndSwap(t.Context(), "k", []byte("a"), 0)
			require.NoError(t, err)
			stop(t, ns)

			ops := map[string]func(context.Context) error{
				"load": func(ctx context.Context) error { _, _, err := b.Load(ctx, "k"); return err },
				"swap": func(ctx context.Context) error {
					_, err := b.CompareAndSwap(ctx, "k", []byte("b"), rev)
					return err
				},
				"delete": func(ctx context.Context) error { return b.CompareAndDelete(ctx, "k", rev) },
			}
			for op, call := range ops {
				// Bounded because a gone server answers nothing at all.
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				err := call(ctx)
				cancel()
				assert.ErrorIs(t, err, kvsync.ErrUnavailable, op)
				assert.NotErrorIs(t, err, kvsync.ErrRevisionMismatch, op)
			}
		})
	}
}

// A recreated sync stream restarts its revisions, so a stale holder's revision
// could match a new key. Losing the stream must leave every write failing.
func TestSyncBucket_LostStreamIsNeverRecreated(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.OpenSyncBucket(js)
	rev, err := b.CompareAndSwap(t.Context(), "k", []byte("a"), 0)
	require.NoError(t, err)
	require.NoError(t, js.DeleteKeyValue(t.Context(), "spinifex-sync"))

	for range 3 {
		_, err = b.CompareAndSwap(t.Context(), "k", []byte("b"), 0)
		require.Error(t, err)
		_, err = b.CompareAndSwap(t.Context(), "k", []byte("b"), rev)
		require.Error(t, err)
		_, _, err = b.Load(t.Context(), "k")
		require.Error(t, err)
	}
	_, err = js.KeyValue(t.Context(), "spinifex-sync")
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "the sync bucket must still be absent")
}

// stallingJS holds every bucket lookup until gate closes, standing in for an
// open stuck on a server that has not formed quorum.
type stallingJS struct {
	jetstream.JetStream

	entered chan struct{}
	once    sync.Once
	gate    chan struct{}
}

func (s *stallingJS) KeyValue(ctx context.Context, name string) (jetstream.KeyValue, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return s.JetStream.KeyValue(ctx, name)
}

// One caller's stuck open must not hold another past its own context, or a
// lease renewal waiting behind it could outlive its deadline.
func TestSyncBucket_AWaiterOnAStuckOpenStopsAtItsContext(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	stall := &stallingJS{JetStream: js, entered: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(func() { close(stall.gate) })
	b := kvstore.OpenSyncBucket(stall)

	go func() { _, _, _ = b.Load(context.Background(), "k") }()
	<-stall.entered

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := b.CompareAndSwap(ctx, "k", []byte("a"), 0)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, kvsync.ErrUnavailable)
	case <-time.After(5 * time.Second):
		t.Fatal("the swap waited on another caller's open past its own context")
	}
}

// leaderlessJS answers every bucket lookup and create with the 503 a cluster
// gives while JetStream has no meta leader.
type leaderlessJS struct {
	jetstream.JetStream
}

func errNoMetaLeader() error {
	return &jetstream.APIError{Code: 503, ErrorCode: 10008, Description: "JetStream system temporarily unavailable"}
}

func (leaderlessJS) KeyValue(context.Context, string) (jetstream.KeyValue, error) {
	return nil, errNoMetaLeader()
}

func (leaderlessJS) CreateKeyValue(context.Context, jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	return nil, errNoMetaLeader()
}

// A cold cluster still electing its meta leader is an outage a lock caller
// waits out, not a fault that ends its election.
func TestSyncBucket_ALeaderlessJetStreamIsUnavailable(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := kvstore.OpenSyncBucket(leaderlessJS{JetStream: js})

	_, _, err := b.Load(t.Context(), "k")
	require.ErrorIs(t, err, kvsync.ErrUnavailable)
	_, err = b.CompareAndSwap(t.Context(), "k", []byte("a"), 0)
	require.ErrorIs(t, err, kvsync.ErrUnavailable)
}
