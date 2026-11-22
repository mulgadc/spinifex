package readiness_test

import (
	"context"
	"sync"
	"testing"
	"time"

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

const markerKey = "_readiness.marker.5be0a2c4"

// observe writes a value marker if the bucket has none and binds to it.
func observe(t *testing.T, js jetstream.JetStream, bucket string) readiness.Binding {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bucket)
	require.NoError(t, err)
	if _, err := kv.Create(t.Context(), markerKey, []byte("5be0a2c4")); err != nil {
		require.ErrorIs(t, err, jetstream.ErrKeyExists)
	}
	b, err := readiness.Observe(t.Context(), js, bucket, markerKey)
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
			_, err = readiness.Observe(t.Context(), js, bucket, markerKey)
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

			_, err = original.Put(ctx, "a", []byte("original"))
			require.NoError(t, err)
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

	b := observe(t, js, readiness.KVBucket)
	for name, mutate := range map[string]func(*readiness.Binding){
		"no bucket":      func(b *readiness.Binding) { b.Bucket = "" },
		"no created":     func(b *readiness.Binding) { b.Created = time.Time{} },
		"no marker key":  func(b *readiness.Binding) { b.Marker.Key = "" },
		"no marker seq":  func(b *readiness.Binding) { b.Marker.Seq = 0 },
		"no marker time": func(b *readiness.Binding) { b.Marker.Time = time.Time{} },
	} {
		incomplete := b
		mutate(&incomplete)
		_, _, err := readiness.Attach(t.Context(), js, incomplete)
		require.ErrorIs(t, err, readiness.ErrInvalid, name)
	}
	_, err := readiness.Observe(t.Context(), js, readiness.KVBucket, "")
	require.ErrorIs(t, err, readiness.ErrInvalid)
}

// hookJS runs a one-shot hook after the next stream info or bucket open, so a
// test can pause a check with its answer in hand or replace the stream mid-open.
type hookJS struct {
	jetstream.JetStream
	mu          sync.Mutex
	afterInfo   func(*jetstream.StreamInfo)
	afterKVOpen func()
}

func (h *hookJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	st, err := h.JetStream.Stream(ctx, name)
	h.mu.Lock()
	hook := h.afterInfo
	h.afterInfo = nil
	h.mu.Unlock()
	if hook != nil && err == nil {
		hook(st.CachedInfo())
	}
	return st, err
}

func (h *hookJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := h.JetStream.KeyValue(ctx, bucket)
	h.mu.Lock()
	hook := h.afterKVOpen
	h.afterKVOpen = nil
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return kv, err
}

func (h *hookJS) arm(afterInfo func(*jetstream.StreamInfo), afterKVOpen func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterInfo, h.afterKVOpen = afterInfo, afterKVOpen
}

func TestStream_DelayedSuccessCannotOutliveAConcurrentQuarantine(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	bucket := "bedrock-endpoints"
	createBucket(t, js, bucket)
	hooked := &hookJS{JetStream: js}
	s, _, err := readiness.Attach(t.Context(), hooked, observe(t, js, bucket))
	require.NoError(t, err)

	answered, resume := make(chan struct{}), make(chan struct{})
	hooked.arm(func(*jetstream.StreamInfo) {
		close(answered)
		<-resume
	}, nil)
	old := make(chan error, 1)
	go func() { old <- s.Verify(t.Context()) }()
	<-answered

	recreate(t, js, bucket)
	require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamReplaced)
	close(resume)
	require.ErrorIs(t, <-old, readiness.ErrStreamReplaced, "an older matching answer must not report success after the quarantine")
}

func TestStream_AnswerWithoutLeaderFailsClosed(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	bucket := "bedrock-endpoints"
	createBucket(t, js, bucket)
	b := observe(t, js, bucket)
	st, err := js.Stream(t.Context(), "KV_"+bucket)
	require.NoError(t, err)
	require.NotNil(t, st.CachedInfo().Cluster)
	require.NotEmpty(t, st.CachedInfo().Cluster.Leader, "a standalone server names itself as leader")

	hooked := &hookJS{JetStream: js}
	s, _, err := readiness.Attach(t.Context(), hooked, b)
	require.NoError(t, err)
	for name, strip := range map[string]func(*jetstream.StreamInfo){
		"leaderless group member": func(info *jetstream.StreamInfo) { info.Cluster.Leader = "" },
		"no cluster section":      func(info *jetstream.StreamInfo) { info.Cluster = nil },
	} {
		hooked.arm(strip, nil)
		require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamUnavailable, name)
		hooked.arm(strip, nil)
		_, err := readiness.Observe(t.Context(), hooked, bucket, markerKey)
		require.ErrorIs(t, err, readiness.ErrStreamUnavailable, name)
	}
	require.NoError(t, s.Verify(t.Context()), "a leaderless answer blocks without quarantining")
}

func TestStream_AttachRefusesReplacementDuringOpen(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	bucket := "bedrock-endpoints"
	createBucket(t, js, bucket)
	b := observe(t, js, bucket)

	hooked := &hookJS{JetStream: js}
	hooked.arm(nil, func() { recreate(t, js, bucket) })
	_, kv, err := readiness.Attach(t.Context(), hooked, b)
	require.ErrorIs(t, err, readiness.ErrStreamReplaced, "the check after opening must catch a replacement made during the open")
	assert.Nil(t, kv)
}

func TestStream_MarkerMayBeAValueOrADeleteMarker(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv := createBucket(t, js, readiness.KVBucket)
	require.NoError(t, kv.Delete(t.Context(), markerKey))
	b, err := readiness.Observe(t.Context(), js, readiness.KVBucket, markerKey)
	require.NoError(t, err)
	s, _, err := readiness.Attach(t.Context(), js, b)
	require.NoError(t, err)

	_, err = kv.Put(t.Context(), "other", []byte("v"))
	require.NoError(t, err)
	require.NoError(t, kv.Delete(t.Context(), "other"))
	require.NoError(t, s.Verify(t.Context()), "writes to other keys leave the marker's position unchanged")
}

func TestStream_ChangedMarkerQuarantines(t *testing.T) {
	for name, change := range map[string]func(*testing.T, jetstream.KeyValue){
		"rewritten with the same bytes": func(t *testing.T, kv jetstream.KeyValue) {
			_, err := kv.Put(t.Context(), markerKey, []byte("5be0a2c4"))
			require.NoError(t, err)
		},
		"deleted": func(t *testing.T, kv jetstream.KeyValue) {
			require.NoError(t, kv.Delete(t.Context(), markerKey))
		},
		"purged by key": func(t *testing.T, kv jetstream.KeyValue) {
			require.NoError(t, kv.Purge(t.Context(), markerKey))
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			kv := createBucket(t, js, readiness.KVBucket)
			b := observe(t, js, readiness.KVBucket)
			s, _, err := readiness.Attach(t.Context(), js, b)
			require.NoError(t, err)

			change(t, kv)
			require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamReplaced)
			_, _, err = readiness.Attach(t.Context(), js, b)
			require.ErrorIs(t, err, readiness.ErrStreamReplaced)
		})
	}
	t.Run("removed from the stream", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		createBucket(t, js, readiness.KVBucket)
		b := observe(t, js, readiness.KVBucket)
		s, _, err := readiness.Attach(t.Context(), js, b)
		require.NoError(t, err)

		st, err := js.Stream(t.Context(), "KV_"+readiness.KVBucket)
		require.NoError(t, err)
		require.NoError(t, st.Purge(t.Context(), jetstream.WithPurgeSubject("$KV."+readiness.KVBucket+"."+markerKey)))
		require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamMissing)
		_, err = js.KeyValue(t.Context(), readiness.KVBucket)
		require.NoError(t, err, "the stream itself survives; only its marker is gone")
	})
}

func TestStream_MarkerCatchesAReplacementReportingTheBoundCreation(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	createBucket(t, js, readiness.KVBucket)
	b := observe(t, js, readiness.KVBucket)
	require.Equal(t, uint64(1), b.Marker.Seq)

	recreate(t, js, readiness.KVBucket)
	replaced := observe(t, js, readiness.KVBucket)
	require.Equal(t, b.Marker.Seq, replaced.Marker.Seq, "the replacement's marker reuses the sequence")

	hooked := &hookJS{JetStream: js}
	var forge func(*jetstream.StreamInfo)
	forge = func(info *jetstream.StreamInfo) {
		info.Created = b.Created
		hooked.arm(forge, nil)
	}
	hooked.arm(forge, nil)
	_, _, err := readiness.Attach(t.Context(), hooked, b)
	require.ErrorIs(t, err, readiness.ErrStreamReplaced, "the marker's storage time differs even when creation and sequence match")
	require.ErrorContains(t, err, "marker")
}

func TestStream_UnreadableMarkerBlocksWithoutQuarantine(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	createBucket(t, js, readiness.KVBucket)
	hooked := &hookJS{JetStream: js}
	s, _, err := readiness.Attach(t.Context(), hooked, observe(t, js, readiness.KVBucket))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	hooked.arm(func(*jetstream.StreamInfo) { cancel() }, nil)
	require.ErrorIs(t, s.Verify(ctx), readiness.ErrStreamUnavailable)
	require.NoError(t, s.Verify(t.Context()))
}

func TestStream_WholeStreamPurgeKeepsCreationButLosesTheMarker(t *testing.T) {
	for _, bucket := range boundBuckets {
		t.Run(bucket, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			kv := createBucket(t, js, bucket)
			_, err := kv.Put(t.Context(), "record", []byte("v"))
			require.NoError(t, err)
			require.NoError(t, kv.Delete(t.Context(), markerKey))
			b, err := readiness.Observe(t.Context(), js, bucket, markerKey)
			require.NoError(t, err)
			s, _, err := readiness.Attach(t.Context(), js, b)
			require.NoError(t, err)

			st, err := js.Stream(t.Context(), "KV_"+bucket)
			require.NoError(t, err)
			require.NoError(t, st.Purge(t.Context()))

			st, err = js.Stream(t.Context(), "KV_"+bucket)
			require.NoError(t, err)
			require.True(t, st.CachedInfo().Created.Equal(b.Created), "a whole-stream purge keeps the creation time")
			require.Zero(t, st.CachedInfo().State.Msgs)
			_, err = kv.Get(t.Context(), "record")
			require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "the purge erased the stored record")

			require.ErrorIs(t, s.Verify(t.Context()), readiness.ErrStreamMissing, "the missing marker refuses the emptied stream")
			_, _, err = readiness.Attach(t.Context(), js, b)
			require.ErrorIs(t, err, readiness.ErrStreamMissing)
		})
	}
}
