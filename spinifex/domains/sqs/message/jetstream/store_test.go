package jetstream_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfig is small enough to enumerate and has fewer state buckets than
// message streams, so one bucket serves several shards.
var testConfig = sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute}

// startServer runs an embedded JetStream server over dir, so a test can stop
// it and start another over the same data.
func startServer(t *testing.T, dir string) (*server.Server, jsapi.JetStream) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true,
	})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second), "server not ready")
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	clustersize.DeclareForTest(t, 1)
	return srv, js
}

func newStore(t *testing.T) (*sqsjs.Store, jsapi.JetStream) {
	t.Helper()
	_, js := startServer(t, t.TempDir())
	s, err := sqsjs.New(context.Background(), js, testConfig)
	require.NoError(t, err)
	return s, js
}

func ns(shard message.Shard, queue string) message.Namespace {
	return message.Namespace{Shard: shard, Queue: message.QueueID(queue)}
}

func streamNames(t *testing.T, js jsapi.JetStream) []string {
	t.Helper()
	var names []string
	lister := js.StreamNames(context.Background())
	for name := range lister.Name() {
		names = append(names, name)
	}
	require.NoError(t, lister.Err())
	slices.Sort(names)
	return names
}

func TestDefaultConfigPerTopology(t *testing.T) {
	assert.Equal(t, sqsjs.Config{MessageStreams: 16, StateBuckets: 8, DuplicateWindow: sqsjs.DefaultDuplicateWindow}, sqsjs.DefaultConfig(1))
	assert.Equal(t, sqsjs.Config{MessageStreams: 8, StateBuckets: 8, DuplicateWindow: sqsjs.DefaultDuplicateWindow}, sqsjs.DefaultConfig(3))
	require.NoError(t, sqsjs.DefaultConfig(1).Validate())
}

func TestConfigValidate(t *testing.T) {
	for name, cfg := range map[string]sqsjs.Config{
		"no streams":           {StateBuckets: 1, DuplicateWindow: time.Minute},
		"no buckets":           {MessageStreams: 1, DuplicateWindow: time.Minute},
		"no window":            {MessageStreams: 1, StateBuckets: 1},
		"window past backstop": {MessageStreams: 1, StateBuckets: 1, DuplicateWindow: message.MinRetentionBackstop + time.Second},
	} {
		assert.ErrorIs(t, cfg.Validate(), message.ErrInvalidArgument, name)
		_, err := sqsjs.New(context.Background(), nil, cfg)
		assert.ErrorIs(t, err, message.ErrInvalidArgument, name)
	}
}

// TestMessageStreamConfigPerReplicaCount covers both topologies' stream shape,
// including R3, which one embedded server cannot host.
func TestMessageStreamConfigPerReplicaCount(t *testing.T) {
	for nodes, want := range map[int]int{1: 1, 3: 3} {
		sc := testConfig.MessageStreamConfig(2, clustersize.ReplicasFor(nodes))
		assert.Equal(t, want, sc.Replicas, "%d nodes", nodes)
		assert.Equal(t, "SQS_M_2", sc.Name)
		assert.Equal(t, []string{"sqs.m.2.>"}, sc.Subjects)
		assert.Equal(t, jsapi.LimitsPolicy, sc.Retention, "ADR-0011:S9: every stream keeps limits retention")
		assert.Equal(t, jsapi.FileStorage, sc.Storage)
		assert.Equal(t, jsapi.DefaultPersistMode, sc.PersistMode, "ADR-0011:S3: an acknowledgement follows a sync")
		assert.Equal(t, 14*24*time.Hour+time.Hour, sc.MaxAge, "ADR-0011:S12 backstop")
		assert.Equal(t, time.Minute, sc.Duplicates)
	}
}

func TestNamesAndKeys(t *testing.T) {
	q := ns(5, "q-1")
	assert.Equal(t, "sqs.m.5.q-1", sqsjs.Subject(q))
	assert.Equal(t, "q.5.q-1.42", sqsjs.StateKey(q, 42))
	assert.Equal(t, "sqs_state_1", sqsjs.BucketName(1))
	assert.Equal(t, 1, testConfig.BucketIndex(5))
}

// TestNewProvisionsOnlyTheFixedLayout holds ADR-0011:S9: no stream, bucket
// or consumer is created per queue.
func TestNewProvisionsOnlyTheFixedLayout(t *testing.T) {
	s, js := newStore(t)
	want := []string{"KV_sqs_state_0", "KV_sqs_state_1", "SQS_M_0", "SQS_M_1", "SQS_M_2"}
	require.Equal(t, want, streamNames(t, js))

	ctx := context.Background()
	for i := range 20 {
		q := ns(message.Shard(i%3), fmt.Sprintf("q%d", i))
		res, err := s.Append(ctx, q, message.AppendInput{Body: []byte("m")})
		require.NoError(t, err)
		_, err = s.PutState(ctx, q, res.Position, []byte("s"), 0)
		require.NoError(t, err)
	}
	assert.Equal(t, want, streamNames(t, js), "queues add no streams or buckets")

	info, err := js.Stream(ctx, "SQS_M_0")
	require.NoError(t, err)
	assert.Equal(t, 1, info.CachedInfo().Config.Replicas, "one node is R1")
	assert.Equal(t, message.MinRetentionBackstop, info.CachedInfo().Config.MaxAge)
	assert.Zero(t, info.CachedInfo().State.Consumers)

	_, err = sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err, "provisioning again is idempotent")
	assert.Equal(t, want, streamNames(t, js))
}

func TestAppendSubjectPerNamespace(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	a, b, otherShard := ns(1, "qa"), ns(1, "qb"), ns(2, "qa")

	ra, err := s.Append(ctx, a, message.AppendInput{Body: []byte("body-a"), Meta: map[string]string{"sent": "1"}})
	require.NoError(t, err)
	rb, err := s.Append(ctx, b, message.AppendInput{Body: []byte("body-b")})
	require.NoError(t, err)
	ro, err := s.Append(ctx, otherShard, message.AppendInput{Body: []byte("body-o")})
	require.NoError(t, err)
	assert.Greater(t, rb.Position, ra.Position, "positions increase within a shard")

	stream, err := js.Stream(ctx, "SQS_M_1")
	require.NoError(t, err)
	got, err := stream.GetMsg(ctx, uint64(ra.Position))
	require.NoError(t, err)
	assert.Equal(t, "sqs.m.1.qa", got.Subject)
	assert.Equal(t, []byte("body-a"), got.Data)
	assert.Equal(t, "1", got.Header.Get("Sqs-Meta-sent"))
	got, err = stream.GetMsg(ctx, uint64(rb.Position))
	require.NoError(t, err)
	assert.Equal(t, "sqs.m.1.qb", got.Subject, "a shard's queues share its stream on separate subjects")

	other, err := js.Stream(ctx, "SQS_M_2")
	require.NoError(t, err)
	got, err = other.GetMsg(ctx, uint64(ro.Position))
	require.NoError(t, err)
	assert.Equal(t, "sqs.m.2.qa", got.Subject, "the same queue ID on another shard lands in that shard's stream")
}

func TestAppendRejectsBadInput(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.Append(ctx, ns(3, "q"), message.AppendInput{})
	require.ErrorIs(t, err, message.ErrInvalidArgument, "shard past the configured streams")
	_, err = s.Append(ctx, ns(0, "a.b"), message.AppendInput{})
	require.ErrorIs(t, err, message.ErrInvalidArgument)
	for _, meta := range []map[string]string{{"": "v"}, {"a:b": "v"}, {"a b": "v"}, {"k": "v\r\n"}} {
		_, err = s.Append(ctx, ns(0, "q"), message.AppendInput{Meta: meta})
		require.ErrorIs(t, err, message.ErrInvalidArgument, "meta %v", meta)
	}
	_, err = s.PutState(ctx, ns(3, "q"), 1, nil, 0)
	require.ErrorIs(t, err, message.ErrInvalidArgument)
	_, err = s.GetState(ctx, ns(3, "q"), 1)
	require.ErrorIs(t, err, message.ErrInvalidArgument)
}

func TestIdempotentAppend(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q, neighbour := ns(0, "qa"), ns(0, "qb")

	first, err := s.Append(ctx, q, message.AppendInput{Body: []byte("one"), IdempotencyKey: "k"})
	require.NoError(t, err)
	require.False(t, first.Duplicate)
	again, err := s.Append(ctx, q, message.AppendInput{Body: []byte("two"), IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.True(t, again.Duplicate)
	assert.Equal(t, first.Position, again.Position)

	elsewhere, err := s.Append(ctx, neighbour, message.AppendInput{Body: []byte("x"), IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.False(t, elsewhere.Duplicate, "a key is scoped to its queue, not the shared stream")

	stream, err := js.Stream(ctx, "SQS_M_0")
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, info.State.Msgs, "the repeat stored nothing")
}

// TestAppendSurvivesRestart: an acknowledged append is on disk, so a server
// started over the same directory still serves it at the same position.
func TestAppendSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	srv, js := startServer(t, dir)
	ctx := context.Background()
	s, err := sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err)
	q := ns(1, "durable")
	res, err := s.Append(ctx, q, message.AppendInput{Body: []byte("kept"), IdempotencyKey: "k"})
	require.NoError(t, err)
	rev, err := s.PutState(ctx, q, res.Position, []byte("state"), 0)
	require.NoError(t, err)
	srv.Shutdown()
	srv.WaitForShutdown()

	_, js = startServer(t, dir)
	s, err = sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "SQS_M_1")
	require.NoError(t, err)
	got, err := stream.GetMsg(ctx, uint64(res.Position))
	require.NoError(t, err)
	assert.Equal(t, []byte("kept"), got.Data)
	st, err := s.GetState(ctx, q, res.Position)
	require.NoError(t, err)
	assert.Equal(t, message.State{Value: []byte("state"), Revision: rev}, st)
	again, err := s.Append(ctx, q, message.AppendInput{Body: []byte("kept"), IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.True(t, again.Duplicate, "the duplicate window survives a restart")
	assert.Equal(t, res.Position, again.Position)
}

func TestStateCompareAndSet(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q := ns(2, "q")
	const pos message.Position = 7

	_, err := s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, message.ErrNotFound)
	r1, err := s.PutState(ctx, q, pos, []byte("v1"), 0)
	require.NoError(t, err)
	_, err = s.PutState(ctx, q, pos, []byte("x"), 0)
	require.ErrorIs(t, err, message.ErrRevisionMismatch)
	r2, err := s.PutState(ctx, q, pos, []byte("v2"), r1)
	require.NoError(t, err)
	_, err = s.PutState(ctx, q, pos, []byte("x"), r1)
	require.ErrorIs(t, err, message.ErrRevisionMismatch)

	kv, err := js.KeyValue(ctx, "sqs_state_0")
	require.NoError(t, err)
	e, err := kv.Get(ctx, "q.2.q.7")
	require.NoError(t, err, "shard 2 of 2 buckets lives in bucket 0 under its namespaced key")
	assert.Equal(t, []byte("v2"), e.Value())

	require.ErrorIs(t, s.DeleteState(ctx, q, pos, r1), message.ErrRevisionMismatch)
	require.ErrorIs(t, s.DeleteState(ctx, q, pos, 0), message.ErrRevisionMismatch, "zero never matches a present record")
	require.NoError(t, s.DeleteState(ctx, q, pos, r2))
	_, err = s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, message.ErrNotFound)
	require.ErrorIs(t, s.DeleteState(ctx, q, pos, r2), message.ErrNotFound)
	_, err = s.PutState(ctx, q, pos, []byte("again"), r2)
	require.ErrorIs(t, err, message.ErrRevisionMismatch, "a deleted record is only recreated from zero")
	_, err = s.PutState(ctx, q, pos, []byte("again"), 0)
	require.NoError(t, err)
}

func TestStateIsolatedAcrossSharedBucket(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	// Shards 0 and 2 share bucket 0 with testConfig's two buckets.
	a, b := ns(0, "q"), ns(2, "q")
	_, err := s.PutState(ctx, a, 1, []byte("a"), 0)
	require.NoError(t, err)
	_, err = s.GetState(ctx, b, 1)
	require.ErrorIs(t, err, message.ErrNotFound)
	_, err = s.PutState(ctx, b, 1, []byte("b"), 0)
	require.NoError(t, err)
	got, err := s.GetState(ctx, a, 1)
	require.NoError(t, err)
	assert.Equal(t, []byte("a"), got.Value)
}

func TestConcurrentStateWrites(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	q := ns(1, "q")
	race := func(expected message.Revision) message.Revision {
		var wg sync.WaitGroup
		var wins atomic.Int32
		var won atomic.Uint64
		var other atomic.Value
		for i := range 16 {
			wg.Go(func() {
				rev, err := s.PutState(ctx, q, 1, fmt.Appendf(nil, "w%d", i), expected)
				switch {
				case err == nil:
					wins.Add(1)
					won.Store(uint64(rev))
				case !errors.Is(err, message.ErrRevisionMismatch):
					other.Store(err)
				}
			})
		}
		wg.Wait()
		require.Nil(t, other.Load(), "losers fail only with ErrRevisionMismatch")
		require.EqualValues(t, 1, wins.Load())
		return message.Revision(won.Load())
	}
	race(race(0))
}
