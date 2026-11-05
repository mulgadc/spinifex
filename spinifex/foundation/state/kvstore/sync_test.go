package kvstore_test

import (
	"context"
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
