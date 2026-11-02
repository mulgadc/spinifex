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

// TestDeleteNamespaceWithUndeclaredStateShardDeletesNothing: a call naming a
// real queue's message shard and queue ID but a state shard with no bucket
// must fail before it purges the queue's bodies.
func TestDeleteNamespaceWithUndeclaredStateShardDeletesNothing(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	valid := ns(1, 1, "q")
	p := appendTo(t, s, valid, "body")
	_, err := s.PutState(ctx, valid, p, []byte("s"), 0)
	require.NoError(t, err)
	before := streamNames(t, js)

	wrong := ns(valid.MessageShard, 7, string(valid.Queue))
	require.Error(t, s.DeleteNamespace(ctx, wrong))

	got, err := s.Get(ctx, valid, p)
	require.NoError(t, err, "the body survives the failed call")
	assert.Equal(t, []byte("body"), got.Body)
	_, err = s.GetState(ctx, valid, p)
	require.NoError(t, err, "the state survives the failed call")
	requireUsage(t, s, valid, message.Usage{Messages: 1, States: 1})
	assert.Equal(t, before, streamNames(t, js), "the resource layout is unchanged")
}

func openFeed(t *testing.T, s *sqsjs.Store, shard message.Shard, after message.Cursor) message.Feed {
	t.Helper()
	f, err := s.Feed(context.Background(), shard, after)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func nextChange(t *testing.T, f message.Feed) message.Change {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := f.Next(ctx)
	require.NoError(t, err)
	return c
}

func requireFeedIdle(t *testing.T, f message.Feed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := f.Next(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func consumerCount(t *testing.T, js jsapi.JetStream, stream string) int {
	t.Helper()
	info, err := js.Stream(context.Background(), stream)
	require.NoError(t, err)
	si, err := info.Info(context.Background())
	require.NoError(t, err)
	return si.State.Consumers
}

// TestFeedReconstructsNamespaces: one shared stream carries several queues
// with different state shards; each change names its complete namespace and
// only domain metadata.
func TestFeedReconstructsNamespaces(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	a, b, other := ns(1, 0, "qa"), ns(1, 1, "qb"), ns(2, 1, "qa")
	pa, err := s.Append(ctx, a, message.AppendInput{Body: []byte("secret-a"), Meta: map[string]string{"sent": "1"}})
	require.NoError(t, err)
	appendTo(t, s, other, "other shard")
	pb := appendTo(t, s, b, "secret-b")

	f := openFeed(t, s, 1, 0)
	assert.Equal(t, message.Change{Namespace: a, Position: pa.Position, Meta: map[string]string{"sent": "1"}, Cursor: message.Cursor(pa.Position)}, nextChange(t, f))
	assert.Equal(t, message.Change{Namespace: b, Position: pb, Cursor: message.Cursor(pb)}, nextChange(t, f))
	requireFeedIdle(t, f)

	got, err := s.Get(ctx, a, pa.Position)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"sent": "1"}, got.Meta, "the internal state shard header is not metadata")
}

// TestFeedDeliversNoBody: the feed is headers-only; a body never reaches it.
func TestFeedDeliversNoBody(t *testing.T) {
	s, js := newStore(t)
	appendTo(t, s, ns(0, 0, "q"), "body bytes")
	f := openFeed(t, s, 0, 0)
	nextChange(t, f)

	stream, err := js.Stream(context.Background(), sqsjs.StreamName(0))
	require.NoError(t, err)
	consumers := stream.ListConsumers(context.Background())
	var seen int
	for info := range consumers.Info() {
		seen++
		assert.True(t, info.Config.HeadersOnly, "headers only")
		assert.Equal(t, jsapi.AckNonePolicy, info.Config.AckPolicy, "no acknowledgement state")
		assert.Empty(t, info.Config.Durable, "ephemeral")
		assert.True(t, info.Config.MemoryStorage, "in memory")
	}
	require.NoError(t, consumers.Err())
	assert.Equal(t, 1, seen)
}

func TestFeedOrderResumeAndDeletedOmission(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	q := ns(0, 1, "q")
	p1, p2, p3 := appendTo(t, s, q, "1"), appendTo(t, s, q, "2"), appendTo(t, s, q, "3")
	require.NoError(t, s.Delete(ctx, q, p2))

	f := openFeed(t, s, 0, 0)
	c1 := nextChange(t, f)
	assert.Equal(t, p1, c1.Position)
	assert.Equal(t, p3, nextChange(t, f).Position, "a deleted body is not fed")
	requireFeedIdle(t, f)

	resumed := openFeed(t, s, 0, c1.Cursor)
	assert.Equal(t, p3, nextChange(t, resumed).Position, "resume starts after the cursor")
	requireFeedIdle(t, resumed)
}

func TestFeedWakesOnLiveAppend(t *testing.T) {
	s, _ := newStore(t)
	q := ns(2, 0, "q")
	f := openFeed(t, s, 2, 0)
	got := make(chan message.Change, 1)
	go func() {
		c, err := f.Next(context.Background())
		if err == nil {
			got <- c
		}
	}()
	time.Sleep(100 * time.Millisecond)
	p := appendTo(t, s, q, "live")
	select {
	case c := <-got:
		assert.Equal(t, message.Change{Namespace: q, Position: p, Cursor: message.Cursor(p)}, c)
	case <-time.After(5 * time.Second):
		t.Fatal("feed did not wake for a live append")
	}
}

func TestFeedNextHonoursContext(t *testing.T) {
	s, _ := newStore(t)
	f := openFeed(t, s, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.Next(ctx)
	require.ErrorIs(t, err, context.Canceled)

	p := appendTo(t, s, ns(0, 0, "q"), "after cancel")
	assert.Equal(t, p, nextChange(t, f).Position, "a cancelled Next does not end the feed")
}

func TestFeedCloseLeavesNoConsumer(t *testing.T) {
	s, js := newStore(t)
	appendTo(t, s, ns(1, 0, "q"), "x")
	f, err := s.Feed(context.Background(), 1, 0)
	require.NoError(t, err)
	nextChange(t, f)
	require.Equal(t, 1, consumerCount(t, js, sqsjs.StreamName(1)))
	require.NoError(t, f.Close())
	assert.Equal(t, 0, consumerCount(t, js, sqsjs.StreamName(1)))
}

func TestFeedRejectsMalformedStateShardHeader(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q := ns(0, 0, "q")
	subject := sqsjs.Subject(q)
	for _, h := range []nats.Header{
		{},
		{"Sqs-State-Shard": []string{"x"}},
		{"Sqs-State-Shard": []string{"1", "2"}},
	} {
		_, err := js.PublishMsg(ctx, &nats.Msg{Subject: subject, Header: h})
		require.NoError(t, err)
	}
	good := appendTo(t, s, q, "good")

	f := openFeed(t, s, 0, 0)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := f.Next(ctx)
		cancel()
		require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
	}
	assert.Equal(t, good, nextChange(t, f).Position, "the feed moves past a corrupt record")
}

func TestFeedOpensRetainedStreamButCreatesNone(t *testing.T) {
	dir := t.TempDir()
	srv, js := startServer(t, dir)
	ctx := context.Background()
	wide, err := sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err)
	old := ns(2, 1, "old")
	p := appendTo(t, wide, old, "kept")
	srv.Shutdown()
	srv.WaitForShutdown()

	_, js = startServer(t, dir)
	narrow, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 1, StateBuckets: 1, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	f := openFeed(t, narrow, 2, 0)
	assert.Equal(t, message.Change{Namespace: old, Position: p, Cursor: message.Cursor(p)}, nextChange(t, f))

	before := streamNames(t, js)
	_, err = narrow.Feed(ctx, 9, 0)
	require.Error(t, err)
	assert.Equal(t, before, streamNames(t, js))
}

// TestFeedAfterMaximumCursorReplaysNothing: no position follows the maximum
// cursor, so the feed must not wrap to the start of the stream.
func TestFeedAfterMaximumCursorReplaysNothing(t *testing.T) {
	s, _ := newStore(t)
	q := ns(0, 0, "q")
	appendTo(t, s, q, "existing")
	f := openFeed(t, s, 0, message.Cursor(math.MaxUint64))
	requireFeedIdle(t, f)
	p := appendTo(t, s, q, "new")
	assert.Equal(t, p, nextChange(t, f).Position, "only a new append is fed")
}

func namespaces(t *testing.T, s *sqsjs.Store, shard message.Shard) []message.NamespaceUsage {
	t.Helper()
	got, err := s.Namespaces(context.Background(), shard)
	require.NoError(t, err)
	return got
}

// TestNamespacesListsCompleteRows covers body and state, state only after
// every body is deleted, bodies only, a deleted state key, two state buckets
// and a neighbouring shard. The store holds no queue records, so every row
// is rebuilt from stored data alone.
func TestNamespacesListsCompleteRows(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	both, stateOnly, bodyOnly, cleared := ns(1, 1, "both"), ns(1, 0, "state-only"), ns(1, 1, "body-only"), ns(1, 0, "cleared")
	p := appendTo(t, s, both, "a")
	appendTo(t, s, both, "b")
	_, err := s.PutState(ctx, both, p, []byte("s"), 0)
	require.NoError(t, err)

	p = appendTo(t, s, stateOnly, "m")
	_, err = s.PutState(ctx, stateOnly, p, []byte("tombstone"), 0)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, stateOnly, p))

	appendTo(t, s, bodyOnly, "m")

	p = appendTo(t, s, cleared, "m")
	rev, err := s.PutState(ctx, cleared, p, []byte("s"), 0)
	require.NoError(t, err)
	require.NoError(t, s.DeleteState(ctx, cleared, p, rev))
	require.NoError(t, s.Delete(ctx, cleared, p))

	appendTo(t, s, ns(2, 1, "both"), "neighbour shard")

	assert.Equal(t, []message.NamespaceUsage{
		{Namespace: bodyOnly, Usage: message.Usage{Messages: 1}},
		{Namespace: both, Usage: message.Usage{Messages: 2, States: 1}},
		{Namespace: stateOnly, Usage: message.Usage{States: 1}},
	}, namespaces(t, s, 1))
	for _, row := range namespaces(t, s, 1) {
		requireUsage(t, s, row.Namespace, row.Usage)
	}
	assert.Empty(t, namespaces(t, s, 0))
}

// TestNamespacesFindsHistoricalBuckets: after a shrink to one stream and one
// bucket, a state-only queue on a historical bucket and a queue on a
// historical stream are still listed, and listing creates nothing.
func TestNamespacesFindsHistoricalBuckets(t *testing.T) {
	dir := t.TempDir()
	srv, js := startServer(t, dir)
	ctx := context.Background()
	wide, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 3, StateBuckets: 3, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	stateOnly, historical := ns(0, 2, "state-only"), ns(2, 1, "historical")
	p := appendTo(t, wide, stateOnly, "m")
	_, err = wide.PutState(ctx, stateOnly, p, []byte("tombstone"), 0)
	require.NoError(t, err)
	require.NoError(t, wide.Delete(ctx, stateOnly, p))
	p = appendTo(t, wide, historical, "m")
	_, err = wide.PutState(ctx, historical, p, []byte("s"), 0)
	require.NoError(t, err)
	srv.Shutdown()
	srv.WaitForShutdown()

	_, js = startServer(t, dir)
	narrow, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 1, StateBuckets: 1, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	before := streamNames(t, js)
	assert.Equal(t, []message.NamespaceUsage{{Namespace: stateOnly, Usage: message.Usage{States: 1}}}, namespaces(t, narrow, 0))
	assert.Equal(t, []message.NamespaceUsage{{Namespace: historical, Usage: message.Usage{Messages: 1, States: 1}}}, namespaces(t, narrow, 2))
	_, err = narrow.Namespaces(ctx, 9)
	require.Error(t, err, "a message shard with no stream")
	assert.Equal(t, before, streamNames(t, js), "listing creates nothing")
}

// TestNamespacesRefusesConflictingAllocations: a queue's allocation is fixed,
// so bodies and state that disagree, or state split across buckets, is
// reported rather than resolved by guessing.
func TestNamespacesRefusesConflictingAllocations(t *testing.T) {
	ctx := context.Background()
	t.Run("BodyAndStateDisagree", func(t *testing.T) {
		s, _ := newStore(t)
		p := appendTo(t, s, ns(1, 0, "q"), "m")
		_, err := s.PutState(ctx, ns(1, 1, "q"), p, []byte("s"), 0)
		require.NoError(t, err)
		_, err = s.Namespaces(ctx, 1)
		require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
	})
	t.Run("StateInTwoBuckets", func(t *testing.T) {
		s, _ := newStore(t)
		for shard := range message.Shard(2) {
			_, err := s.PutState(ctx, ns(1, shard, "q"), 1, []byte("s"), 0)
			require.NoError(t, err)
		}
		_, err := s.Namespaces(ctx, 1)
		require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
	})
	t.Run("FirstAndLastBodyDisagree", func(t *testing.T) {
		s, _ := newStore(t)
		appendTo(t, s, ns(1, 0, "q"), "m")
		appendTo(t, s, ns(1, 1, "q"), "m")
		_, err := s.Namespaces(ctx, 1)
		require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
	})
	t.Run("MalformedStateKey", func(t *testing.T) {
		s, js := newStore(t)
		kv, err := js.KeyValue(ctx, sqsjs.BucketName(0))
		require.NoError(t, err)
		_, err = kv.Put(ctx, "q.1.q.x", []byte("s"))
		require.NoError(t, err)
		_, err = s.Namespaces(ctx, 1)
		require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
	})
}

// TestNamespacesRefusesAMiddleBodyWithAnotherStateShard: every body is
// checked, not only the first and last.
func TestNamespacesRefusesAMiddleBodyWithAnotherStateShard(t *testing.T) {
	s, _ := newStore(t)
	appendTo(t, s, ns(1, 0, "q"), "first")
	appendTo(t, s, ns(1, 1, "q"), "middle")
	appendTo(t, s, ns(1, 0, "q"), "last")
	_, err := s.Namespaces(context.Background(), 1)
	require.ErrorIs(t, err, sqsjs.ErrCorruptRecord)
}

// TestNamespacesLeavesNoConsumer: the enumeration pass removes its consumer.
func TestNamespacesLeavesNoConsumer(t *testing.T) {
	s, js := newStore(t)
	for i := range 600 {
		appendTo(t, s, ns(1, 0, fmt.Sprintf("q%d", i%3)), "m")
	}
	got := namespaces(t, s, 1)
	require.Len(t, got, 3)
	for _, row := range got {
		assert.Equal(t, uint64(200), row.Usage.Messages)
	}
	assert.Equal(t, 0, consumerCount(t, js, sqsjs.StreamName(1)))
}

// Get must be a leader read: a stream that allows direct get, as an earlier
// configuration's shard might, would let any replica answer with a stale
// body. The store refuses the read rather than serve one.
func TestGetRefusesDirectGetStream(t *testing.T) {
	ctx := context.Background()
	_, js := startServer(t, t.TempDir())
	s, err := sqsjs.New(ctx, js, testConfig)
	require.NoError(t, err)
	assert.False(t, testConfig.MessageStreamConfig(0, 1).AllowDirect, "the store never provisions direct get")

	earlier := message.Shard(testConfig.MessageStreams)
	cfg := testConfig.MessageStreamConfig(earlier, 1)
	cfg.AllowDirect = true
	_, err = js.CreateStream(ctx, cfg)
	require.NoError(t, err)
	n := ns(earlier, 0, "directq")
	ack, err := js.Publish(ctx, sqsjs.Subject(n), []byte("b"))
	require.NoError(t, err)

	_, err = s.Get(ctx, n, message.Position(ack.Sequence))
	require.Error(t, err)
	assert.NotErrorIs(t, err, message.ErrNotFound, "a refused read is never taken as removed")
}
