package jetstream_test

import (
	"context"
	"strings"
	"sync"
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

// apiRecorder records every JetStream API request subject the server sees,
// in the order one client sent them.
type apiRecorder struct {
	mu       sync.Mutex
	subjects []string
	sentinel chan struct{}
}

const recorderSentinel = "sqs.test.recorder.sentinel"

func recordAPI(t *testing.T, url string) *apiRecorder {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	r := &apiRecorder{sentinel: make(chan struct{}, 1)}
	// One channel for both subjects keeps arrival order; separate async
	// subscriptions deliver on separate goroutines and can reorder.
	msgs := make(chan *nats.Msg, 1<<12)
	_, err = nc.ChanSubscribe("$JS.API.>", msgs)
	require.NoError(t, err)
	_, err = nc.ChanSubscribe(recorderSentinel, msgs)
	require.NoError(t, err)
	go func() {
		for m := range msgs {
			if m.Subject == recorderSentinel {
				r.sentinel <- struct{}{}
				continue
			}
			r.mu.Lock()
			r.subjects = append(r.subjects, m.Subject)
			r.mu.Unlock()
		}
	}()
	require.NoError(t, nc.Flush())
	return r
}

// drain waits until the recorder has seen everything the client sent before
// the sentinel, then returns and clears the recorded subjects.
func (r *apiRecorder) drain(t *testing.T, client *nats.Conn) []string {
	t.Helper()
	require.NoError(t, client.Publish(recorderSentinel, nil))
	require.NoError(t, client.Flush())
	select {
	case <-r.sentinel:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder sentinel not delivered")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.subjects
	r.subjects = nil
	return got
}

// TestGetStateIsLeaderServed pins the route: a state read is a stream
// message get, which only the stream leader answers, never a direct get,
// which a follower may answer from state it has not yet applied.
func TestGetStateIsLeaderServed(t *testing.T) {
	srv, js := startServer(t, t.TempDir())
	s, err := sqsjs.New(context.Background(), js, testConfig)
	require.NoError(t, err)
	ctx := context.Background()
	q := ns(0, 1, "route")
	pos := appendTo(t, s, q, "b")
	rev, err := s.PutState(ctx, q, pos, []byte("v"), 0)
	require.NoError(t, err)

	rec := recordAPI(t, srv.ClientURL())
	rec.drain(t, js.Conn())
	_, err = s.GetState(ctx, q, pos)
	require.NoError(t, err)
	require.NoError(t, s.DeleteState(ctx, q, pos, rev))
	_, err = s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, message.ErrNotFound)
	_, err = s.GetState(ctx, q, pos+1)
	require.ErrorIs(t, err, message.ErrNotFound)

	leaderGets := 0
	for _, subj := range rec.drain(t, js.Conn()) {
		assert.False(t, strings.HasPrefix(subj, "$JS.API.DIRECT.GET."), "state read used a direct get: %s", subj)
		if subj == "$JS.API.STREAM.MSG.GET.KV_"+sqsjs.BucketName(1) {
			leaderGets++
		}
	}
	// Three GetState calls, plus the one DeleteState makes first.
	assert.Equal(t, 4, leaderGets)
}

// TestGetStateSemantics covers each way a record can be absent, and that a
// present one carries the value and revision its last write returned.
func TestGetStateSemantics(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q := ns(1, 0, "sem")
	pos := appendTo(t, s, q, "b")

	_, err := s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, message.ErrNotFound, "never written")

	rev, err := s.PutState(ctx, q, pos, []byte("one"), 0)
	require.NoError(t, err)
	rev, err = s.PutState(ctx, q, pos, []byte("two"), rev)
	require.NoError(t, err)
	got, err := s.GetState(ctx, q, pos)
	require.NoError(t, err)
	assert.Equal(t, message.State{Value: []byte("two"), Revision: rev}, got)

	empty := pos + 1
	erev, err := s.PutState(ctx, q, empty, []byte{}, 0)
	require.NoError(t, err)
	got, err = s.GetState(ctx, q, empty)
	require.NoError(t, err)
	assert.Empty(t, got.Value)
	assert.Equal(t, erev, got.Revision)

	require.NoError(t, s.DeleteState(ctx, q, pos, rev))
	_, err = s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, message.ErrNotFound, "deleted")

	// A KV purge leaves a marker carrying a PURGE operation.
	kv, err := js.KeyValue(ctx, sqsjs.BucketName(0))
	require.NoError(t, err)
	require.NoError(t, kv.Purge(ctx, sqsjs.StateKey(q, empty)))
	_, err = s.GetState(ctx, q, empty)
	require.ErrorIs(t, err, message.ErrNotFound, "purged key")

	again := pos + 2
	_, err = s.PutState(ctx, q, again, []byte("x"), 0)
	require.NoError(t, err)
	require.NoError(t, s.DeleteNamespace(ctx, q))
	_, err = s.GetState(ctx, q, again)
	require.ErrorIs(t, err, message.ErrNotFound, "namespace deleted")
}

func TestGetStateErrors(t *testing.T) {
	s, _ := newStore(t)
	q := ns(0, 0, "errs")
	pos := appendTo(t, s, q, "b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.GetState(ctx, q, pos)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, message.ErrNotFound)

	_, err = s.GetState(context.Background(), message.Namespace{MessageShard: 0, StateShard: 0}, pos)
	require.Error(t, err, "invalid namespace")
	assert.NotErrorIs(t, err, message.ErrNotFound)

	// testConfig provisions two state buckets; shard 7 has none.
	_, err = s.GetState(context.Background(), ns(0, 7, "errs"), pos)
	require.Error(t, err)
	assert.NotErrorIs(t, err, message.ErrNotFound)
}

// TestGetStateHonoursAPIPrefix reads state through a JetStream context
// built for a domain and for an explicit prefix.
func TestGetStateHonoursAPIPrefix(t *testing.T) {
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, JetStreamDomain: "hub",
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)
	clustersize.DeclareForTest(t, 1)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	byDomain, err := jsapi.NewWithDomain(nc, "hub")
	require.NoError(t, err)
	byPrefix, err := jsapi.NewWithAPIPrefix(nc, "$JS.hub.API")
	require.NoError(t, err)
	ctx := context.Background()
	for name, js := range map[string]jsapi.JetStream{"domain": byDomain, "prefix": byPrefix} {
		s, err := sqsjs.New(ctx, js, testConfig)
		require.NoError(t, err, name)
		q := ns(0, 0, "pfx-"+name)
		pos := appendTo(t, s, q, "b")
		rev, err := s.PutState(ctx, q, pos, []byte("v"), 0)
		require.NoError(t, err, name)
		got, err := s.GetState(ctx, q, pos)
		require.NoError(t, err, name)
		assert.Equal(t, message.State{Value: []byte("v"), Revision: rev}, got, name)
	}
}
