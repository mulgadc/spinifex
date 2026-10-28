package jetstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wedgedIterator models a consumer whose pull has stopped delivering: every
// Next waits out its window and returns nothing, as the live one does.
type wedgedIterator struct {
	wait    time.Duration
	nexts   atomic.Int32
	stopped atomic.Bool
}

var _ jsapi.MessagesContext = (*wedgedIterator)(nil)

func (w *wedgedIterator) Next(...jsapi.NextOpt) (jsapi.Msg, error) {
	w.nexts.Add(1)
	time.Sleep(w.wait)
	return nil, context.DeadlineExceeded
}

func (w *wedgedIterator) Stop()  { w.stopped.Store(true) }
func (w *wedgedIterator) Drain() { w.stopped.Store(true) }

func feedStore(t *testing.T) (*Store, jsapi.JetStream) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	clustersize.DeclareForTest(t, 1)
	s, err := New(context.Background(), js, Config{MessageStreams: 1, StateBuckets: 1, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	return s, js
}

func appendAt(t *testing.T, s *Store, ns message.Namespace) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), ns, message.AppendInput{Body: []byte("m")})
	require.NoError(t, err)
	return res.Position
}

// openWedgeable opens a feed with a short idle check, so recovery is quick.
func openWedgeable(t *testing.T, s *Store) *feed {
	t.Helper()
	fd, err := s.Feed(context.Background(), 0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fd.Close() })
	f := fd.(*feed)
	f.idleCheck = 100 * time.Millisecond
	return f
}

// wedge swaps the feed's live iterator for one that never delivers.
func wedge(f *feed) *wedgedIterator {
	f.it.Stop()
	w := &wedgedIterator{wait: f.idleCheck}
	f.it = w
	return w
}

func consumerNames(t *testing.T, s *Store) []string {
	t.Helper()
	stream, err := s.js.Stream(context.Background(), StreamName(0))
	require.NoError(t, err)
	var names []string
	lister := stream.ConsumerNames(context.Background())
	for name := range lister.Name() {
		names = append(names, name)
	}
	require.NoError(t, lister.Err())
	return names
}

// TestFeedRecoversFromWedgedPull: with a change stored after the cursor and a
// consumer that delivers nothing, Next replaces the consumer from the cursor
// and returns that change within one idle check plus the reopen.
func TestFeedRecoversFromWedgedPull(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	first, second := appendAt(t, s, ns), appendAt(t, s, ns)
	f := openWedgeable(t, s)

	c, err := f.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, c.Position)
	wedged := f.consumer.CachedInfo().Name
	w := wedge(f)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c, err = f.Next(ctx)
	require.NoError(t, err)
	assert.Equal(t, second, c.Position, "the next change after the cursor, none skipped or repeated")
	assert.Less(t, time.Since(start), 2*f.idleCheck+time.Second, "recovery is bounded by the idle check and one reopen")
	assert.True(t, w.stopped.Load(), "the wedged iterator is stopped")
	assert.EqualValues(t, 1, w.nexts.Load(), "one idle check finds the stored change")
	assert.NotContains(t, consumerNames(t, s), wedged, "the wedged consumer is deleted, not left to linger")
	assert.Len(t, consumerNames(t, s), 1)

	live := appendAt(t, s, ns)
	c, err = f.Next(ctx)
	require.NoError(t, err)
	assert.Equal(t, live, c.Position, "the replacement consumer delivers live appends")
}

// TestFeedIdleWithNothingStoredKeepsConsumer: an idle feed caught up with the
// stream waits on its consumer and never rebuilds it.
func TestFeedIdleWithNothingStoredKeepsConsumer(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	appendAt(t, s, ns)
	f := openWedgeable(t, s)
	_, err := f.Next(context.Background())
	require.NoError(t, err)
	name := f.consumer.CachedInfo().Name
	w := wedge(f)

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, w.nexts.Load(), int32(3), "the feed keeps waiting, one idle check after another")
	assert.False(t, w.stopped.Load(), "nothing stored after the cursor, so no rebuild")
	assert.Equal(t, []string{name}, consumerNames(t, s))
}

// detachForTest takes the live consumer as tracked work, for discard.
func (f *feed) detachForTest() (jsapi.Consumer, jsapi.MessagesContext) {
	return f.detach()
}
