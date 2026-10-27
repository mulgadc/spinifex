package jetstream_test

import (
	"context"
	"errors"
	"fmt"
	"math"
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

func ns(messageShard, stateShard message.Shard, queue string) message.Namespace {
	return message.Namespace{MessageShard: messageShard, StateShard: stateShard, Queue: message.QueueID(queue)}
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
	q := ns(5, 1, "q-1")
	assert.Equal(t, "sqs.m.5.q-1", sqsjs.Subject(q))
	assert.Equal(t, "q.5.q-1.42", sqsjs.StateKey(q, 42))
	assert.Equal(t, "sqs_state_1", sqsjs.BucketName(1))
}

// TestNewProvisionsOnlyTheFixedLayout holds ADR-0011:S9: no stream, bucket
// or consumer is created per queue.
func TestNewProvisionsOnlyTheFixedLayout(t *testing.T) {
	s, js := newStore(t)
	want := []string{"KV_sqs_state_0", "KV_sqs_state_1", "SQS_M_0", "SQS_M_1", "SQS_M_2"}
	require.Equal(t, want, streamNames(t, js))

	ctx := context.Background()
	for i := range 20 {
		q := ns(message.Shard(i%3), message.Shard(i%2), fmt.Sprintf("q%d", i))
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
	for _, bucket := range []string{"sqs_state_0", "sqs_state_1"} {
		kv, err := js.KeyValue(ctx, bucket)
		require.NoError(t, err)
		status, err := kv.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, message.MinRetentionBackstop, status.TTL(), "%s has the state backstop", bucket)
		assert.Equal(t, 1, status.Config().Replicas, "%s is R1 on one node", bucket)
	}

	_, err = sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err, "provisioning again is idempotent")
	assert.Equal(t, want, streamNames(t, js))
}

func TestAppendSubjectPerNamespace(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	a, b, otherShard := ns(1, 0, "qa"), ns(1, 1, "qb"), ns(2, 0, "qa")

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
	_, err := s.Append(ctx, ns(0, 0, "a.b"), message.AppendInput{})
	require.ErrorIs(t, err, message.ErrInvalidArgument)
	for _, meta := range []map[string]string{{"": "v"}, {"a:b": "v"}, {"a b": "v"}, {"k": "v\r\n"}} {
		_, err = s.Append(ctx, ns(0, 0, "q"), message.AppendInput{Meta: meta})
		require.ErrorIs(t, err, message.ErrInvalidArgument, "meta %v", meta)
	}
	_, err = s.PutState(ctx, ns(0, 0, "a.b"), 1, nil, 0)
	require.ErrorIs(t, err, message.ErrInvalidArgument)
	_, err = s.GetState(ctx, ns(0, 0, "a.b"), 1)
	require.ErrorIs(t, err, message.ErrInvalidArgument)
}

// TestAllocationWithNoResourceFails: an allocation no configuration ever
// provisioned is an error, never a silent create or a not-found.
func TestAllocationWithNoResourceFails(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	_, err := s.Append(ctx, ns(7, 0, "q"), message.AppendInput{Body: []byte("m")})
	require.Error(t, err)
	_, err = s.GetState(ctx, ns(0, 7, "q"), 1)
	require.Error(t, err)
	require.NotErrorIs(t, err, message.ErrNotFound)
	_, err = s.PutState(ctx, ns(0, 7, "q"), 1, []byte("s"), 0)
	require.Error(t, err)
	assert.Equal(t, []string{"KV_sqs_state_0", "KV_sqs_state_1", "SQS_M_0", "SQS_M_1", "SQS_M_2"}, streamNames(t, js), "nothing was created for the allocation")
}

func TestIdempotentAppend(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q, neighbour := ns(0, 0, "qa"), ns(0, 1, "qb")

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
	q := ns(1, 1, "durable")
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
	q := ns(2, 1, "q")
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

	kv, err := js.KeyValue(ctx, "sqs_state_1")
	require.NoError(t, err)
	e, err := kv.Get(ctx, "q.2.q.7")
	require.NoError(t, err, "state lives in the bucket its StateShard names, keyed by message shard, queue and position")
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
	// Same queue ID and state bucket, different message shards: a position
	// is unique only within a message stream.
	a, b := ns(0, 0, "q"), ns(2, 0, "q")
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
	q := ns(1, 1, "q")
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

// TestTopologyTransitionKeepsExistingAllocations holds ADR-0011:S10: a
// changed count applies to new queues, and an existing queue keeps the
// stream and bucket its record names.
func TestTopologyTransitionKeepsExistingAllocations(t *testing.T) {
	_, js := startServer(t, t.TempDir())
	ctx := context.Background()
	before, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 3, StateBuckets: 3, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	old := ns(2, 2, "old")
	first, err := before.Append(ctx, old, message.AppendInput{Body: []byte("first")})
	require.NoError(t, err)
	rev, err := before.PutState(ctx, old, first.Position, []byte("claimed"), 0)
	require.NoError(t, err)

	after, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 1, StateBuckets: 1, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, []string{"KV_sqs_state_0", "KV_sqs_state_1", "KV_sqs_state_2", "SQS_M_0", "SQS_M_1", "SQS_M_2"}, streamNames(t, js), "a reduced count deletes nothing")

	second, err := after.Append(ctx, old, message.AppendInput{Body: []byte("second")})
	require.NoError(t, err, "an existing queue still appends to its retained stream")
	assert.Greater(t, second.Position, first.Position)
	stream, err := js.Stream(ctx, "SQS_M_2")
	require.NoError(t, err)
	got, err := stream.GetMsg(ctx, uint64(second.Position))
	require.NoError(t, err)
	assert.Equal(t, "sqs.m.2.old", got.Subject)

	st, err := after.GetState(ctx, old, first.Position)
	require.NoError(t, err, "an existing queue still reads its retained bucket")
	assert.Equal(t, message.State{Value: []byte("claimed"), Revision: rev}, st)
	_, err = after.PutState(ctx, old, first.Position, []byte("visible"), rev)
	require.NoError(t, err)
	_, err = after.PutState(ctx, old, second.Position, []byte("new"), 0)
	require.NoError(t, err)

	fresh := ns(0, 0, "fresh")
	_, err = after.Append(ctx, fresh, message.AppendInput{Body: []byte("m")})
	require.NoError(t, err, "a new queue uses the current layout")

	read, err := after.Get(ctx, old, first.Position)
	require.NoError(t, err, "an existing queue still reads its retained stream")
	assert.Equal(t, []byte("first"), read.Body)
	requireUsage(t, after, old, message.Usage{Messages: 2, States: 2})
	require.NoError(t, after.DeleteThrough(ctx, old, first.Position))
	requireUsage(t, after, old, message.Usage{Messages: 1, States: 2})
	require.NoError(t, after.DeleteNamespace(ctx, old), "cleanup reaches the retained stream and bucket")
	requireUsage(t, after, old, message.Usage{})
}

// TestNewRefusesUnsupportedClusterSize: only one node (R1) and three (R3)
// are supported, so no other size reaches provisioning, which would
// otherwise create R5 on five nodes.
func TestNewRefusesUnsupportedClusterSize(t *testing.T) {
	_, js := startServer(t, t.TempDir())
	for _, nodes := range []int{2, 4, 5} {
		t.Run(fmt.Sprintf("%d nodes", nodes), func(t *testing.T) {
			clustersize.RedeclareForTest(t, nodes)
			_, err := sqsjs.New(context.Background(), js, testConfig)
			require.ErrorIs(t, err, sqsjs.ErrUnsupportedTopology)
			assert.Empty(t, streamNames(t, js), "nothing is provisioned")
		})
	}
	clustersize.ResetForTest()
	t.Cleanup(func() { clustersize.DeclareForTest(t, 1) })
	_, err := sqsjs.New(context.Background(), js, testConfig)
	require.ErrorIs(t, err, sqsjs.ErrUnsupportedTopology, "an undeclared size is refused too")
}

func appendTo(t *testing.T, s *sqsjs.Store, q message.Namespace, body string) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), q, message.AppendInput{Body: []byte(body)})
	require.NoError(t, err)
	return res.Position
}

func requireUsage(t *testing.T, s *sqsjs.Store, q message.Namespace, want message.Usage) {
	t.Helper()
	got, err := s.Count(context.Background(), q)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestGet(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := ns(1, 0, "qa"), ns(1, 1, "qb")
	res, err := s.Append(ctx, a, message.AppendInput{Body: []byte("body"), Meta: map[string]string{"sent": "1", "delay": "0"}})
	require.NoError(t, err)
	plain := appendTo(t, s, a, "plain")
	neighbour := appendTo(t, s, b, "theirs")

	got, err := s.Get(ctx, a, res.Position)
	require.NoError(t, err)
	assert.Equal(t, message.Message{Position: res.Position, Body: []byte("body"), Meta: map[string]string{"sent": "1", "delay": "0"}}, got)
	got, err = s.Get(ctx, a, plain)
	require.NoError(t, err)
	assert.Nil(t, got.Meta, "no metadata reads back as none")

	_, err = s.Get(ctx, a, neighbour)
	require.ErrorIs(t, err, message.ErrNotFound, "a position on a neighbour's subject is not this namespace's")
	_, err = s.Get(ctx, a, neighbour+100)
	require.ErrorIs(t, err, message.ErrNotFound)
}

func TestDeleteIsNamespaceScopedAndIdempotent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := ns(0, 0, "qa"), ns(0, 0, "qb")
	pa := appendTo(t, s, a, "a")
	pb := appendTo(t, s, b, "b")

	require.NoError(t, s.Delete(ctx, a, pb), "a neighbour's position is absent from this namespace")
	_, err := s.Get(ctx, b, pb)
	require.NoError(t, err, "and the neighbour's message is untouched")

	require.NoError(t, s.Delete(ctx, a, pa))
	_, err = s.Get(ctx, a, pa)
	require.ErrorIs(t, err, message.ErrNotFound)
	require.NoError(t, s.Delete(ctx, a, pa), "deleting again succeeds")
	requireUsage(t, s, a, message.Usage{})
	requireUsage(t, s, b, message.Usage{Messages: 1})
}

func TestDeleteThroughIsSubjectScoped(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b := ns(2, 1, "qa"), ns(2, 1, "qb")
	a1 := appendTo(t, s, a, "a1")
	b1 := appendTo(t, s, b, "b1")
	a2 := appendTo(t, s, a, "a2")
	b2 := appendTo(t, s, b, "b2")
	a3 := appendTo(t, s, a, "a3")
	_, err := s.PutState(ctx, a, a1, []byte("s"), 0)
	require.NoError(t, err)

	require.NoError(t, s.DeleteThrough(ctx, a, 0), "through zero removes nothing")
	requireUsage(t, s, a, message.Usage{Messages: 3, States: 1})

	require.NoError(t, s.DeleteThrough(ctx, a, a2))
	for _, p := range []message.Position{a1, a2} {
		_, err := s.Get(ctx, a, p)
		require.ErrorIs(t, err, message.ErrNotFound, "position %d is at or before the range end", p)
	}
	_, err = s.Get(ctx, a, a3)
	require.NoError(t, err, "a later body stays")
	for _, p := range []message.Position{b1, b2} {
		_, err := s.Get(ctx, b, p)
		require.NoError(t, err, "a neighbour's body inside the range stays")
	}
	_, err = s.GetState(ctx, a, a1)
	require.NoError(t, err, "state records stay")
	requireUsage(t, s, a, message.Usage{Messages: 1, States: 1})

	require.NoError(t, s.DeleteThrough(ctx, a, math.MaxUint64))
	requireUsage(t, s, a, message.Usage{States: 1})
	requireUsage(t, s, b, message.Usage{Messages: 2})
}

func TestCountExcludesDeletedStateAndNeighbours(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	// Same stream and state bucket, so only the subject and key prefix
	// separate them.
	a, b := ns(1, 1, "qa"), ns(1, 1, "qb")
	requireUsage(t, s, a, message.Usage{})
	var pos []message.Position
	var revs []message.Revision
	for i := range 3 {
		p := appendTo(t, s, a, fmt.Sprintf("a%d", i))
		pos = append(pos, p)
		if i < 2 {
			rev, err := s.PutState(ctx, a, p, []byte("s"), 0)
			require.NoError(t, err)
			revs = append(revs, rev)
		}
	}
	pb := appendTo(t, s, b, "b")
	_, err := s.PutState(ctx, b, pb, []byte("s"), 0)
	require.NoError(t, err)
	requireUsage(t, s, a, message.Usage{Messages: 3, States: 2})

	require.NoError(t, s.DeleteState(ctx, a, pos[0], revs[0]))
	requireUsage(t, s, a, message.Usage{Messages: 3, States: 1})
	requireUsage(t, s, b, message.Usage{Messages: 1, States: 1})

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.Count(cancelled, a)
	require.Error(t, err, "a cancelled count is an error, never a short count")
}

func TestDeleteNamespaceRemovesOnlyItsData(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	a, b := ns(0, 1, "qa"), ns(0, 1, "qb")
	pa := appendTo(t, s, a, "a")
	pa2 := appendTo(t, s, a, "a2")
	pb := appendTo(t, s, b, "b")
	rev, err := s.PutState(ctx, a, pa, []byte("s"), 0)
	require.NoError(t, err)
	_, err = s.PutState(ctx, a, pa2, []byte("s"), 0)
	require.NoError(t, err)
	require.NoError(t, s.DeleteState(ctx, a, pa, rev), "leaves a delete marker")
	_, err = s.PutState(ctx, b, pb, []byte("s"), 0)
	require.NoError(t, err)

	require.NoError(t, s.DeleteNamespace(ctx, a))
	requireUsage(t, s, a, message.Usage{})
	state, err := js.Stream(ctx, "KV_sqs_state_1")
	require.NoError(t, err)
	info, err := state.Info(ctx, jsapi.WithSubjectFilter("$KV.sqs_state_1.q.0.qa.>"))
	require.NoError(t, err)
	assert.Empty(t, info.State.Subjects, "no state record or delete marker of the namespace remains")

	requireUsage(t, s, b, message.Usage{Messages: 1, States: 1})
	require.NoError(t, s.DeleteNamespace(ctx, a), "deleting again succeeds")
}

// TestUndeclaredAllocationCreatesNothing: every read and cleanup on an
// allocation no configuration provisioned fails and creates no resource.
func TestUndeclaredAllocationCreatesNothing(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	before := streamNames(t, js)
	for _, q := range []message.Namespace{ns(7, 0, "q"), ns(0, 7, "q")} {
		_, err := s.Count(ctx, q)
		assert.Error(t, err, "Count %+v", q)
		assert.Error(t, s.DeleteNamespace(ctx, q), "DeleteNamespace %+v", q)
	}
	missingStream := ns(7, 0, "q")
	_, err := s.Get(ctx, missingStream, 1)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, message.ErrNotFound)
	assert.Error(t, s.Delete(ctx, missingStream, 1))
	assert.Error(t, s.DeleteThrough(ctx, missingStream, 1))
	assert.Equal(t, before, streamNames(t, js))
}

func TestLimits(t *testing.T) {
	s, _ := newStore(t)
	assert.Equal(t, message.Limits{RetentionBackstop: message.MinRetentionBackstop, IdempotencyWindow: testConfig.DuplicateWindow}, s.Limits())
}
