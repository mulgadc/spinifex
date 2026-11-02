package jetstream_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var receiveKeys = func() *message.ReceiptKeys {
	k, err := message.NewReceiptKeys(1, map[uint32][]byte{1: bytes.Repeat([]byte{7}, message.MinReceiptKeyLen)})
	if err != nil {
		panic(err)
	}
	return k
}()

// clock is a shared test clock for a sender and its receivers.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type receiveEnv struct {
	sendEnv

	clock *clock
}

func newReceiveEnv(t *testing.T) receiveEnv {
	t.Helper()
	return receiveEnv{sendEnv: newSendEnv(t), clock: &clock{at: fixedNow}}
}

func (e receiveEnv) receiver(store message.Store) *message.Receiver {
	if store == nil {
		store = e.store
	}
	return &message.Receiver{Store: store, Queues: e.queues, Keys: receiveKeys, Now: e.clock.now}
}

func (e receiveEnv) send(t *testing.T, name, body string, delay *int) message.SendResult {
	t.Helper()
	in := input(name, body)
	in.DelaySeconds = delay
	s := &message.Sender{Store: e.store, Queues: e.queues, Now: e.clock.now}
	res, err := s.Send(context.Background(), in)
	require.NoError(t, err)
	return res
}

func recv(name string, n int) message.ReceiveInput {
	return message.ReceiveInput{Account: acct, Region: region, Queue: name, MaxNumberOfMessages: &n}
}

func TestReceiveReturnsSentMessageWithHandle(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", map[string]string{"VisibilityTimeout": "30"})
	sent := e.send(t, "orders", "hello", nil)
	r := e.receiver(nil)

	got, err := r.Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, got, 1)
	m := got[0]
	assert.Equal(t, sent.MessageID, m.MessageID)
	assert.Equal(t, []byte("hello"), m.Body)
	assert.Equal(t, sent.MD5OfBody, m.MD5OfBody)
	assert.Equal(t, sender, m.SenderID)
	assert.Equal(t, sent.EnqueuedAt, m.SentAt)
	assert.Equal(t, uint64(1), m.ReceiveCount)
	assert.Equal(t, fixedNow.Add(30*time.Second).UnixMilli(), m.Deadline.UnixMilli())

	rc, err := receiveKeys.Decode(m.ReceiptHandle, q.Namespace().Queue)
	require.NoError(t, err)
	assert.Equal(t, message.Receipt{Queue: q.Namespace().Queue, Position: sent.Position, Generation: 1}, rc)

	again, err := r.Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Empty(t, again, "an in-flight message is not received twice")

	e.clock.advance(30 * time.Second)
	back, err := r.Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	require.Len(t, back, 1, "visible again at its deadline")
	assert.Equal(t, uint64(2), back[0].ReceiveCount)
	assert.Equal(t, fixedNow.UnixMilli(), back[0].FirstReceive.UnixMilli())
	assert.NotEqual(t, m.ReceiptHandle, back[0].ReceiptHandle)
	rc2, err := receiveKeys.Decode(back[0].ReceiptHandle, q.Namespace().Queue)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), rc2.Generation)
}

func TestReceiveMaxNumberOfMessages(t *testing.T) {
	e := newReceiveEnv(t)
	e.create(t, acct, "orders", nil)
	for i := range 15 {
		e.send(t, "orders", "m"+strconv.Itoa(i), nil)
	}
	r := e.receiver(nil)

	one, err := r.Receive(context.Background(), message.ReceiveInput{Account: acct, Region: region, Queue: "orders"})
	require.NoError(t, err)
	assert.Len(t, one, 1, "default 1")
	ten, err := r.Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Len(t, ten, 10)
	rest, err := r.Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Len(t, rest, 4)

	seen := map[string]bool{}
	for _, m := range append(append(one, ten...), rest...) {
		assert.False(t, seen[m.MessageID], "no message is returned twice")
		seen[m.MessageID] = true
	}
	assert.Len(t, seen, 15)

	for _, n := range []int{0, -1, 11} {
		_, err := r.Receive(context.Background(), recv("orders", n))
		require.ErrorIs(t, err, message.ErrInvalidParameter, "max %d", n)
	}
	for _, v := range []int{-1, message.MaxVisibilitySeconds + 1} {
		in := recv("orders", 1)
		in.VisibilityTimeout = ptr(v)
		_, err := r.Receive(context.Background(), in)
		require.ErrorIs(t, err, message.ErrInvalidParameter, "visibility %d", v)
	}
	for _, w := range []int{-1, message.MaxWaitSeconds + 1} {
		in := recv("orders", 1)
		in.WaitTimeSeconds = ptr(w)
		_, err = r.Receive(context.Background(), in)
		require.ErrorIs(t, err, message.ErrInvalidParameter, "wait %d", w)
	}
}

func TestReceiveIsolatesQueues(t *testing.T) {
	e := newReceiveEnv(t)
	e.create(t, acct, "orders", nil)
	e.create(t, acct, "audit", nil)
	e.create(t, other, "orders", nil)
	e.send(t, "audit", "audit-only", nil)
	in := input("orders", "other-account")
	in.Account = other
	_, err := (&message.Sender{Store: e.store, Queues: e.queues, Now: e.clock.now}).Send(context.Background(), in)
	require.NoError(t, err)

	got, err := e.receiver(nil).Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Empty(t, got, "neither a sibling queue's nor another account's messages")

	_, err = e.receiver(nil).Receive(context.Background(), recv("missing", 1))
	require.ErrorIs(t, err, message.ErrQueueDoesNotExist)
}

// A queue whose first visible message lies beyond one receive's scan budget
// is reached by the next receive, which resumes the pass.
func TestReceiveResumesBeyondScanBudget(t *testing.T) {
	e := newReceiveEnv(t)
	e.create(t, acct, "orders", nil)
	budget := message.ReceiveScanPage * message.ReceiveScanPages
	for i := range budget + 20 {
		e.send(t, "orders", "d"+strconv.Itoa(i), ptr(message.MaxDelaySeconds))
	}
	last := e.send(t, "orders", "ready", nil)
	counted := &scanCounter{Store: e.store}
	r := e.receiver(counted)

	got, err := r.Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	assert.Empty(t, got, "the first receive's budget ends before the message")
	assert.LessOrEqual(t, counted.calls, message.ReceiveScanPages, "scan work per receive is bounded")

	counted.calls = 0
	got, err = r.Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, got, 1, "the next receive resumes the pass")
	assert.Equal(t, last.MessageID, got[0].MessageID)
	assert.LessOrEqual(t, counted.calls, message.ReceiveScanPages)
}

func TestReceiveCompetingConsumers(t *testing.T) {
	e := newReceiveEnv(t)
	e.create(t, acct, "orders", nil)
	for i := range 20 {
		e.send(t, "orders", "m"+strconv.Itoa(i), nil)
	}
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for range 4 {
		r := e.receiver(nil)
		wg.Go(func() {
			for range 5 {
				got, err := r.Receive(context.Background(), recv("orders", 10))
				assert.NoError(t, err)
				mu.Lock()
				for _, m := range got {
					seen[m.MessageID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.Len(t, seen, 20, "every message is received")
	for id, n := range seen {
		assert.Equal(t, 1, n, "message %s received once within its visibility", id)
	}
}

// A scan failure or an unsettled claim is never an empty queue; a receive
// that claimed something still returns it.
func TestReceiveReportsUnsettledOutcomes(t *testing.T) {
	t.Run("scan fails", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		e.send(t, "orders", "hello", nil)
		r := e.receiver(&receiveFaults{Store: e.store, scanErr: errors.New("timeout")})
		_, err := r.Receive(context.Background(), recv("orders", 1))
		require.ErrorIs(t, err, message.ErrUnavailable)
	})
	t.Run("only claim is ambiguous", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		e.send(t, "orders", "hello", nil)
		r := e.receiver(&receiveFaults{Store: e.store, putFails: 1})
		got, err := r.Receive(context.Background(), recv("orders", 1))
		require.ErrorIs(t, err, message.ErrUnavailable)
		assert.Empty(t, got)
	})
	t.Run("another claim succeeds", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		e.send(t, "orders", "first", nil)
		second := e.send(t, "orders", "second", nil)
		r := e.receiver(&receiveFaults{Store: e.store, putFails: 1})
		got, err := r.Receive(context.Background(), recv("orders", 10))
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, second.MessageID, got[0].MessageID)
	})
	t.Run("newer record", func(t *testing.T) {
		e := newReceiveEnv(t)
		q := e.create(t, acct, "orders", nil)
		sent := e.send(t, "orders", "hello", nil)
		_, err := e.store.PutState(context.Background(), q.Namespace(), sent.Position, []byte(`{"v":99}`), 0)
		require.NoError(t, err)
		_, err = e.receiver(nil).Receive(context.Background(), recv("orders", 1))
		require.Error(t, err, "a record this release cannot act on is not an empty queue")
	})
}

type scanCounter struct {
	message.Store

	calls int
}

func (s *scanCounter) Scan(ctx context.Context, shard message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	s.calls++
	return s.Store.Scan(ctx, shard, from, limit)
}

// receiveFaults fails every scan with scanErr, or the first putFails state
// writes after they apply, as a lost acknowledgement would.
type receiveFaults struct {
	message.Store

	scanErr  error
	putFails int
}

func (f *receiveFaults) Scan(ctx context.Context, shard message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	if f.scanErr != nil {
		return message.ScanPage{}, f.scanErr
	}
	return f.Store.Scan(ctx, shard, from, limit)
}

func (f *receiveFaults) PutState(ctx context.Context, ns message.Namespace, pos message.Position, v []byte, rev message.Revision) (message.Revision, error) {
	r, err := f.Store.PutState(ctx, ns, pos, v, rev)
	if err == nil && f.putFails > 0 {
		f.putFails--
		return 0, errors.New("acknowledgement lost")
	}
	return r, err
}
