package jetstream_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// longPollEnv runs on the wall clock: a long poll waits in real time.
type longPollEnv struct {
	queues *queue.Store
	store  message.Store
	js     jetstream.JetStream
}

// Long enough that only the wake feed or a known visibility can end a
// test's wait in time.
const noRecheck = time.Minute

func newLongPollEnv(t *testing.T) longPollEnv {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	qs, err := queue.Open(context.Background(), js, 3, 2)
	require.NoError(t, err)
	ms, mjs := openStoreJS(t, storeMaxPayload)
	return longPollEnv{queues: qs, store: ms, js: mjs}
}

func (e longPollEnv) create(t *testing.T, name string, attrs map[string]string) {
	t.Helper()
	_, err := e.queues.Create(context.Background(), queue.CreateRequest{Account: acct, Region: region, Name: name, Attributes: attrs})
	require.NoError(t, err)
}

func (e longPollEnv) receiver(store message.Store, recheck time.Duration) *message.Receiver {
	if store == nil {
		store = e.store
	}
	return &message.Receiver{Store: store, Queues: e.queues, Keys: receiveKeys, Recheck: recheck}
}

func (e longPollEnv) send(t *testing.T, name, body string, delay *int) message.SendResult {
	t.Helper()
	in := input(name, body)
	in.DelaySeconds = delay
	res, err := (&message.Sender{Store: e.store, Queues: e.queues}).Send(context.Background(), in)
	require.NoError(t, err)
	return res
}

func wait(name string, secs int) message.ReceiveInput {
	in := recv(name, 1)
	in.WaitTimeSeconds = ptr(secs)
	return in
}

// noConsumers asserts every wake feed was released on every message shard.
func (e longPollEnv) noConsumers(t *testing.T) {
	t.Helper()
	for i := range 3 {
		assert.Zero(t, consumerCount(t, e.js, sqsjs.StreamName(message.Shard(i))), "shard %d", i)
	}
}

type pollResult struct {
	got  []message.Received
	err  error
	took time.Duration
}

// scanned signals once its first Scan has returned: the receiver's wake
// feed is open and its first poll's bound is fixed, so it is now waiting.
type scanned struct {
	message.Store

	once *sync.Once
	done chan struct{}
}

func signalScan(s message.Store) scanned {
	return scanned{Store: s, once: new(sync.Once), done: make(chan struct{})}
}

func (s scanned) Scan(ctx context.Context, shard message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	page, err := s.Store.Scan(ctx, shard, from, limit)
	s.once.Do(func() { close(s.done) })
	return page, err
}

func (s scanned) polled(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the receiver never completed a scan")
	}
}

func goReceive(r *message.Receiver, ctx context.Context, in message.ReceiveInput) <-chan pollResult {
	out := make(chan pollResult, 1)
	go func() {
		start := time.Now()
		got, err := r.Receive(ctx, in)
		out <- pollResult{got, err, time.Since(start)}
	}()
	return out
}

// A send from a separate sender wakes a waiting receiver without a recheck.
func TestLongPollWakesOnSend(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	s := signalScan(e.store)
	res := goReceive(e.receiver(s, noRecheck), context.Background(), wait("orders", 10))
	s.polled(t)
	sent := e.send(t, "orders", "hello", nil)

	r := <-res
	require.NoError(t, r.err)
	require.Len(t, r.got, 1)
	assert.Equal(t, sent.MessageID, r.got[0].MessageID)
	assert.Less(t, r.took, 3*time.Second, "woken by the append, not the wait's end")
	e.noConsumers(t)
}

// An append to another queue on the same shard wakes nothing it can take,
// so the waiter keeps waiting and times out empty.
func TestLongPollIgnoresOtherQueues(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	e.create(t, "other", nil)
	s := signalScan(e.store)
	res := goReceive(e.receiver(s, noRecheck), context.Background(), wait("orders", 1))
	s.polled(t)
	e.send(t, "other", "not yours", nil)

	r := <-res
	require.NoError(t, r.err)
	assert.Empty(t, r.got)
	assert.GreaterOrEqual(t, r.took, time.Second)
}

func TestLongPollEmptyTimesOut(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	start := time.Now()
	got, err := e.receiver(nil, 0).Receive(context.Background(), wait("orders", 1))
	require.NoError(t, err)
	assert.Empty(t, got)
	took := time.Since(start)
	assert.GreaterOrEqual(t, took, time.Second)
	assert.Less(t, took, 3*time.Second, "the wait is bounded")
	e.noConsumers(t)
}

// Two waiting receivers on separate instances: one send is taken once.
func TestLongPollCompetingReceivers(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	sa, sb := signalScan(e.store), signalScan(e.store)
	a := goReceive(e.receiver(sa, noRecheck), context.Background(), wait("orders", 2))
	b := goReceive(e.receiver(sb, noRecheck), context.Background(), wait("orders", 2))
	sa.polled(t)
	sb.polled(t)
	sent := e.send(t, "orders", "one", nil)

	ra, rb := <-a, <-b
	require.NoError(t, ra.err)
	require.NoError(t, rb.err)
	got := append(ra.got, rb.got...)
	require.Len(t, got, 1, "exactly one receiver takes the message")
	assert.Equal(t, sent.MessageID, got[0].MessageID)
	assert.Less(t, min(ra.took, rb.took), 2*time.Second, "the taker was woken")
	e.noConsumers(t)
}

func TestLongPollStopsOnCancel(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	ctx, cancel := context.WithCancel(context.Background())
	s := signalScan(e.store)
	res := goReceive(e.receiver(s, noRecheck), ctx, wait("orders", 20))
	s.polled(t)
	cancel()

	r := <-res
	require.ErrorIs(t, r.err, context.Canceled)
	assert.Empty(t, r.got)
	assert.Less(t, r.took, 2*time.Second, "stops promptly")
	e.noConsumers(t)
}

// lostFeed opens a real wake feed, then fails Next once lose is closed.
type lostFeed struct {
	message.Store

	lose chan struct{}
}

func (s lostFeed) Feed(ctx context.Context, shard message.Shard, after message.Cursor) (message.Feed, error) {
	f, err := s.Store.Feed(ctx, shard, after)
	if err != nil {
		return nil, err
	}
	return failingFeed{Feed: f, lose: s.lose}, nil
}

type failingFeed struct {
	message.Feed

	lose chan struct{}
}

func (f failingFeed) Next(ctx context.Context) (message.Change, error) {
	select {
	case <-f.lose:
		return message.Change{}, errors.New("feed lost")
	case <-ctx.Done():
		return message.Change{}, ctx.Err()
	}
}

// A wake feed that fails after opening ends the wait as unavailable, not
// as a successful empty receive.
func TestLongPollFeedFailureAfterOpen(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)
	lose := make(chan struct{})
	s := signalScan(lostFeed{Store: e.store, lose: lose})
	res := goReceive(e.receiver(s, noRecheck), context.Background(), wait("orders", 20))
	s.polled(t)
	close(lose)

	r := <-res
	require.ErrorIs(t, r.err, message.ErrUnavailable)
	assert.Empty(t, r.got)
	assert.Less(t, r.took, 2*time.Second)
	e.noConsumers(t)
}

// heldStore holds Feed or Scan until its context ends; scans hold only
// after the first heldAfter complete.
type heldStore struct {
	message.Store

	feed      bool
	heldAfter int
	// heldErr, when set, replaces the context error a held scan returns.
	heldErr error
	mu      sync.Mutex
	scans   int
}

func (s *heldStore) Feed(ctx context.Context, shard message.Shard, after message.Cursor) (message.Feed, error) {
	if s.feed {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Store.Feed(ctx, shard, after)
}

func (s *heldStore) Scan(ctx context.Context, shard message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	s.mu.Lock()
	s.scans++
	n := s.scans
	s.mu.Unlock()
	if s.heldAfter >= 0 && n > s.heldAfter {
		<-ctx.Done()
		if s.heldErr != nil {
			return message.ScanPage{}, s.heldErr
		}
		return message.ScanPage{}, ctx.Err()
	}
	return s.Store.Scan(ctx, shard, from, limit)
}

// A scan held past the wait ends it on time: empty after a completed poll,
// unavailable when no poll ever completed. No wake consumer is left.
func TestLongPollHeldScan(t *testing.T) {
	t.Parallel()
	e := newLongPollEnv(t)
	e.create(t, "orders", nil)

	// The two waits are independent, so they run side by side.
	after := &heldStore{Store: e.store, heldAfter: 1}
	never := &heldStore{Store: e.store, heldAfter: 0}
	afterRes := goReceive(e.receiver(after, 100*time.Millisecond), context.Background(), wait("orders", 1))
	neverRes := goReceive(e.receiver(never, noRecheck), context.Background(), wait("orders", 1))

	r := <-afterRes
	require.NoError(t, r.err)
	assert.Empty(t, r.got)
	assert.Less(t, r.took, 2*time.Second, "bounded by the wait")

	r = <-neverRes
	require.ErrorIs(t, r.err, message.ErrUnavailable)
	assert.Empty(t, r.got)
	assert.Less(t, r.took, 2*time.Second, "bounded by the wait")
	e.noConsumers(t)
}
