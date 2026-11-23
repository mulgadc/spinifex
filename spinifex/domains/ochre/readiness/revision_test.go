package readiness_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const revisionKey = "000000000000/bW9kZWw"

func revisionEnv(t *testing.T) (jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return js, createBucket(t, js, endpointBucket)
}

func readRevision(t *testing.T, js jetstream.JetStream) readiness.SubjectRevision {
	t.Helper()
	rev, err := readiness.ReadRevision(t.Context(), js, endpointBucket, revisionKey)
	require.NoError(t, err)
	return rev
}

// lastSeq is the stream's last sequence, which moves only when a write lands.
func lastSeq(t *testing.T, js jetstream.JetStream) uint64 {
	t.Helper()
	st, err := js.Stream(t.Context(), "KV_"+endpointBucket)
	require.NoError(t, err)
	info, err := st.Info(t.Context())
	require.NoError(t, err)
	return info.State.LastSeq
}

func TestReadRevision_AbsentKeyIsRevisionZero(t *testing.T) {
	t.Parallel()
	js, _ := revisionEnv(t)
	assert.Equal(t, readiness.SubjectRevision{}, readRevision(t, js))

	rev, err := readiness.WriteAt(t.Context(), js, endpointBucket, revisionKey, []byte("v1"), 0)
	require.NoError(t, err)
	got := readRevision(t, js)
	assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: []byte("v1")}, got)
	assert.True(t, got.Live())
}

// An ordinary Get reports a deleted or purged key as absent; the leader read
// reports its marker's revision, which is the only one a write can land at.
func TestReadRevision_TombstoneKeepsItsRevision(t *testing.T) {
	t.Parallel()
	for op, remove := range map[string]func(context.Context, jetstream.KeyValue) error{
		readiness.OpDel:   func(ctx context.Context, kv jetstream.KeyValue) error { return kv.Delete(ctx, revisionKey) },
		readiness.OpPurge: func(ctx context.Context, kv jetstream.KeyValue) error { return kv.Purge(ctx, revisionKey) },
	} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			js, kv := revisionEnv(t)
			_, err := kv.Put(t.Context(), revisionKey, []byte("old"))
			require.NoError(t, err)
			require.NoError(t, remove(t.Context(), kv))
			_, err = kv.Get(t.Context(), revisionKey)
			require.ErrorIs(t, err, jetstream.ErrKeyNotFound)

			tomb := readRevision(t, js)
			assert.Equal(t, op, tomb.Op)
			assert.NotZero(t, tomb.Seq)
			assert.False(t, tomb.Live())
			assert.Empty(t, tomb.Value)

			pub := &pubJS{JetStream: js}
			_, err = readiness.WriteAt(t.Context(), pub, endpointBucket, revisionKey, []byte("new"), 0)
			require.ErrorIs(t, err, readiness.ErrChanged, "zero must not land over a tombstone")
			assert.Equal(t, 1, pub.count("$KV."+endpointBucket+"."), "one attempt, no retry at the tombstone revision")
			assert.Equal(t, tomb, readRevision(t, js), "a refused write leaves the tombstone")

			rev, err := readiness.WriteAt(t.Context(), js, endpointBucket, revisionKey, []byte("new"), tomb.Seq)
			require.NoError(t, err)
			assert.Equal(t, readiness.SubjectRevision{Seq: rev, Op: readiness.OpPut, Value: []byte("new")}, readRevision(t, js))
		})
	}
}

// A KV Create over a tombstone retries at the tombstone's revision and lands;
// that hidden second write is what a fixed-revision write must not make.
func TestWriteAt_ZeroIsNotACreate(t *testing.T) {
	t.Parallel()
	js, kv := revisionEnv(t)
	_, err := kv.Put(t.Context(), revisionKey, []byte("old"))
	require.NoError(t, err)
	require.NoError(t, kv.Delete(t.Context(), revisionKey))
	_, err = kv.Create(t.Context(), revisionKey, []byte("created"))
	require.NoError(t, err, "Create lands over the tombstone")
	require.NoError(t, kv.Delete(t.Context(), revisionKey))
	tomb := readRevision(t, js)

	_, err = readiness.WriteAt(t.Context(), js, endpointBucket, revisionKey, []byte("new"), 0)
	require.ErrorIs(t, err, readiness.ErrChanged)
	assert.Equal(t, tomb, readRevision(t, js))
}

func TestWriteAt_StaleRevisionIsADefiniteConflict(t *testing.T) {
	t.Parallel()
	js, kv := revisionEnv(t)
	first, err := kv.Put(t.Context(), revisionKey, []byte("a"))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), revisionKey, []byte("b"))
	require.NoError(t, err)
	current := readRevision(t, js)

	for _, stale := range []uint64{0, first, current.Seq + 1} {
		_, err := readiness.WriteAt(t.Context(), js, endpointBucket, revisionKey, []byte("c"), stale)
		require.ErrorIs(t, err, readiness.ErrChanged)
		assert.NotErrorIs(t, err, readiness.ErrOutcomeUnknown)
	}
	assert.Equal(t, current, readRevision(t, js))
}

// A lost acknowledgement is unknown whether or not the write landed; either way
// it is one publish, with no retry and no fresh revision adopted.
func TestWriteAt_LostAcknowledgementIsUnknownAndNotRetried(t *testing.T) {
	t.Parallel()
	for name, land := range map[string]bool{"landed": true, "not landed": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			js, kv := revisionEnv(t)
			_, err := kv.Put(t.Context(), revisionKey, []byte("old"))
			require.NoError(t, err)
			before := readRevision(t, js)
			seq := lastSeq(t, js)
			pub := &pubJS{JetStream: js, land: land, match: func(*nats.Msg) bool { return true }}

			_, err = readiness.WriteAt(t.Context(), pub, endpointBucket, revisionKey, []byte("new"), before.Seq)
			require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
			assert.NotErrorIs(t, err, readiness.ErrChanged)
			assert.Equal(t, 1, pub.count("$KV."+endpointBucket+"."), "exactly one attempt")

			after := readRevision(t, js)
			if land {
				assert.Equal(t, readiness.SubjectRevision{Seq: seq + 1, Op: readiness.OpPut, Value: []byte("new")}, after)
				assert.Equal(t, seq+1, lastSeq(t, js), "only the one landed write")
				return
			}
			assert.Equal(t, before, after)
			assert.Equal(t, seq, lastSeq(t, js), "nothing landed")
		})
	}
}

// A publish answered "no responders" is retried by the client by default. The
// subscriber below counts what reaches the wire: WriteAt sends exactly one.
func TestWriteAt_ClientRetriesAreDisabled(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	const bucket = "no-stream-bucket"
	var delivered atomic.Int32
	sub, err := nc.Subscribe("$KV."+bucket+".>", func(m *nats.Msg) {
		delivered.Add(1)
		reply := nats.NewMsg(m.Reply)
		reply.Header.Set("Status", "503")
		_ = m.RespondMsg(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	_, err = js.PublishMsg(t.Context(), &nats.Msg{Subject: "$KV." + bucket + ".probe", Data: []byte("x")},
		jetstream.WithRetryWait(time.Millisecond))
	require.Error(t, err)
	require.Greater(t, delivered.Load(), int32(1), "the default publish retries, so the count can see a retry")

	delivered.Store(0)
	_, err = readiness.WriteAt(t.Context(), js, bucket, revisionKey, []byte("x"), 0)
	require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)
	assert.EqualValues(t, 1, delivered.Load())
}

func TestRevision_RefusesInvalidOrReservedKeys(t *testing.T) {
	t.Parallel()
	js, _ := revisionEnv(t)
	for _, key := range []string{"", "a b", "a.*", "a.>", ".a", "a.", readiness.MarkerPrefix + "x"} {
		_, err := readiness.ReadRevision(t.Context(), js, endpointBucket, key)
		require.ErrorIs(t, err, readiness.ErrInvalid, key)
		_, err = readiness.WriteAt(t.Context(), js, endpointBucket, key, nil, 0)
		require.ErrorIs(t, err, readiness.ErrInvalid, key)
	}
}

func TestReadRevision_MissingStream(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	_, err := readiness.ReadRevision(t.Context(), js, "absent-bucket", revisionKey)
	require.ErrorIs(t, err, readiness.ErrStreamMissing)
}
