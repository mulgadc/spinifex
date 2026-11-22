package readiness_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundBuckets are the two streams N binds: endpoint records and the control record.
var boundBuckets = []string{"bedrock-endpoints", readiness.KVBucket}

func createBucket(t *testing.T, js jetstream.JetStream, bucket string) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)
	return kv
}

// recreate deletes the bucket's stream and creates a replacement with the same
// configuration, as an out-of-band reset or a delayed get-or-create would.
func recreate(t *testing.T, js jetstream.JetStream, bucket string) jetstream.KeyValue {
	t.Helper()
	require.NoError(t, js.DeleteKeyValue(t.Context(), bucket))
	return createBucket(t, js, bucket)
}

func observe(t *testing.T, js jetstream.JetStream, bucket string) readiness.Binding {
	t.Helper()
	b, err := readiness.Observe(t.Context(), js, bucket)
	require.NoError(t, err)
	return b
}

func TestStream_AttachVerifiesBoundStream(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			createBucket(t, js, bucket)
			b := observe(t, js, bucket)

			s, kv, err := readiness.Attach(t.Context(), js, b)
			require.NoError(t, err)
			require.NoError(t, s.Verify(t.Context()))
			_, err = kv.Put(t.Context(), "k", []byte("v"))
			require.NoError(t, err)
			require.NoError(t, s.Verify(t.Context()), "an acknowledged write on the bound stream verifies")
		})
	}
}

func TestStream_AttachNeverCreatesMissingStream(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			createBucket(t, js, bucket)
			b := observe(t, js, bucket)
			require.NoError(t, js.DeleteKeyValue(t.Context(), bucket))

			_, _, err := readiness.Attach(t.Context(), js, b)
			require.ErrorIs(t, err, readiness.ErrStreamMissing)
			_, err = readiness.Observe(t.Context(), js, bucket)
			require.ErrorIs(t, err, readiness.ErrStreamMissing)
			_, err = js.KeyValue(t.Context(), bucket)
			require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "attaching must not recreate the stream")
		})
	}
}

func TestStream_AttachRefusesRecreatedStream(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			createBucket(t, js, bucket)
			b := observe(t, js, bucket)
			recreate(t, js, bucket)

			_, _, err := readiness.Attach(t.Context(), js, b)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced)
		})
	}
}

func TestStream_LossDuringUseQuarantinesTheHandle(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			createBucket(t, js, bucket)
			b := observe(t, js, bucket)
			s, _, err := readiness.Attach(t.Context(), js, b)
			require.NoError(t, err)

			require.NoError(t, js.DeleteKeyValue(t.Context(), bucket))
			require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamMissing)
			createBucket(t, js, bucket)
			require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamMissing, "a replacement does not lift the quarantine")

			_, _, err = readiness.Attach(t.Context(), js, b)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced, "a reopen under the old binding refuses the replacement")
		})
	}
}

func TestStream_DelayedWritesLandInReplacementThatStaysUnusable(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			ctx := t.Context()
			original := createBucket(t, js, bucket)
			b := observe(t, js, bucket)
			s, delayed, err := readiness.Attach(ctx, js, b)
			require.NoError(t, err)

			for _, key := range []string{"a", "b"} {
				_, err = original.Put(ctx, key, []byte("original"))
				require.NoError(t, err)
			}
			expected, err := original.Put(ctx, "acct.model", []byte("original"))
			require.NoError(t, err)
			require.Equal(t, uint64(3), expected)

			replacement := recreate(t, js, bucket)
			for _, key := range []string{"x", "y"} {
				_, err = replacement.Put(ctx, key, []byte("replacement"))
				require.NoError(t, err)
			}
			reused, err := replacement.Put(ctx, "acct.model", []byte("replacement"))
			require.NoError(t, err)
			require.Equal(t, expected, reused, "the replacement reuses the revision")

			_, err = delayed.Update(ctx, "acct.model", []byte("stale"), expected)
			require.NoError(t, err, "the broker accepts a delayed fixed-revision update on the replacement")
			_, err = delayed.Create(ctx, "acct.new", []byte("stale"))
			require.NoError(t, err, "and a delayed create")
			entry, err := replacement.Get(ctx, "acct.model")
			require.NoError(t, err)
			assert.Equal(t, "stale", string(entry.Value()))

			require.ErrorIs(t, s.Verify(ctx), readiness.ErrStreamReplaced, "the acknowledged write must not be treated as authoritative")
			_, _, err = readiness.Attach(ctx, js, b)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced)
		})
	}
}

func TestStream_UnreadableIdentityBlocksWithoutQuarantine(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	createBucket(t, js, readiness.KVBucket)
	s, _, err := readiness.Attach(t.Context(), js, observe(t, js, readiness.KVBucket))
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.Verify(cancelled), readiness.ErrStreamUnavailable)
	require.NoError(t, s.Verify(t.Context()), "an unreadable identity is retried, not quarantined")
}

func TestStream_AttachRefusesIncompleteBinding(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	createBucket(t, js, readiness.KVBucket)

	_, _, err := readiness.Attach(t.Context(), js, readiness.Binding{Bucket: readiness.KVBucket})
	require.ErrorIs(t, err, readiness.ErrInvalid)
}
