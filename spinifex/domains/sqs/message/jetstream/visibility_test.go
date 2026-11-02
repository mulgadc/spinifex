package jetstream_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e receiveEnv) changer(store message.Store) *message.VisibilityChanger {
	if store == nil {
		store = e.store
	}
	return &message.VisibilityChanger{Store: store, Queues: e.queues, Keys: receiveKeys, Now: e.clock.now}
}

func vis(name, handle string, seconds int) message.ChangeVisibilityInput {
	return message.ChangeVisibilityInput{Account: acct, Region: region, Queue: name, ReceiptHandle: handle, VisibilityTimeout: seconds}
}

func TestChangeVisibilityExtendsDeadline(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", map[string]string{"VisibilityTimeout": "30"})
	m := e.receiveOne(t, "orders")
	before, _ := e.state(t, q, m.ReceiptHandle)

	e.clock.advance(10 * time.Second)
	require.NoError(t, e.changer(nil).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 120)))
	st, _ := e.state(t, q, m.ReceiptHandle)
	assert.EqualValues(t, fixedNow.Add(130*time.Second).UnixMilli(), st["d"], "deadline is now plus the timeout")
	assert.Equal(t, before["g"], st["g"], "the generation is unchanged")
	assert.Equal(t, before["r"], st["r"])
	assert.Equal(t, message.StatusInflight, st["s"])

	e.clock.advance(119 * time.Second)
	got, err := e.receiver(nil).Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Empty(t, got, "still hidden past the queue's 30 seconds")

	e.clock.advance(time.Second)
	got, err = e.receiver(nil).Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Len(t, got, 1, "visible at the new deadline")
}

// The cap is the issuing receive's time plus 12 hours, inclusive, and does
// not reset when the deadline is extended.
func TestChangeVisibilityTwelveHourCap(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", map[string]string{"VisibilityTimeout": "30"})
	m := e.receiveOne(t, "orders")
	c := e.changer(nil)

	require.NoError(t, c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 43200)), "exactly the cap")
	st, _ := e.state(t, q, m.ReceiptHandle)
	assert.EqualValues(t, fixedNow.Add(12*time.Hour).UnixMilli(), st["d"])

	e.clock.advance(time.Second)
	_, before := e.state(t, q, m.ReceiptHandle)
	require.ErrorIs(t, c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 43200)), message.ErrVisibilityCap)
	_, after := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, before, after, "a refused change writes nothing")

	require.NoError(t, c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 43199)), "the remaining time is allowed")

	e.clock.advance(6 * time.Hour)
	require.ErrorIs(t, c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 6*3600+1)), message.ErrVisibilityCap,
		"an earlier extension does not reset the cap")
	require.NoError(t, c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 6*3600-1)))
}

func TestChangeVisibilityRefusesInvalidHandles(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	e.create(t, acct, "audit", nil)
	e.create(t, other, "orders", nil)
	m := e.receiveOne(t, "orders")
	_, before := e.state(t, q, m.ReceiptHandle)
	for name, in := range map[string]message.ChangeVisibilityInput{
		"malformed":         vis("orders", "not-a-handle", 60),
		"tampered":          vis("orders", m.ReceiptHandle[:len(m.ReceiptHandle)-2]+"AA", 60),
		"another queue":     vis("audit", m.ReceiptHandle, 60),
		"another account's": {Account: other, Region: region, Queue: "orders", ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 60},
	} {
		require.ErrorIs(t, e.changer(nil).ChangeVisibility(context.Background(), in), message.ErrReceiptInvalid, name)
	}
	_, after := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, before, after)
}

// A delete landing between the read and the write wins; the change retries,
// finds the tombstone and reports not in flight.
func TestChangeVisibilityRacingDelete(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	m := e.receiveOne(t, "orders")
	racer := &deleteFirst{Store: e.store, d: e.deleter(nil), in: del("orders", m.ReceiptHandle)}
	require.ErrorIs(t, e.changer(racer).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60)), message.ErrNotInflight)
	require.True(t, racer.done)
	st, _ := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, message.StatusDeleted, st["s"], "the change never overwrote the tombstone")
}

// Concurrent changes of one handle each apply, or fail closed once their
// bounded retries are spent; the record ends at one of the requested
// deadlines with its generation intact.
func TestChangeVisibilityConcurrent(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	m := e.receiveOne(t, "orders")
	var (
		wg sync.WaitGroup
		mu sync.Mutex
		ok int
	)
	for i := range 6 {
		c := e.changer(nil)
		wg.Go(func() {
			err := c.ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60+i))
			if err != nil {
				assert.ErrorIs(t, err, message.ErrUnavailable, "a lost race is never reported as settled")
				return
			}
			mu.Lock()
			ok++
			mu.Unlock()
		})
	}
	wg.Wait()
	assert.Positive(t, ok, "at least one change applies")
	st, _ := e.state(t, q, m.ReceiptHandle)
	assert.EqualValues(t, 1, st["g"])
	d := int64(st["d"].(float64))
	assert.GreaterOrEqual(t, d, fixedNow.Add(60*time.Second).UnixMilli())
	assert.LessOrEqual(t, d, fixedNow.Add(65*time.Second).UnixMilli())
}

func TestChangeVisibilityUnsettledOutcomes(t *testing.T) {
	t.Run("lost reply, applied", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		f := &receiveFaults{Store: e.store, putFails: 1}
		require.ErrorIs(t, e.changer(f).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60)), message.ErrUnavailable)
	})
	t.Run("lost reply, not applied, not resent", func(t *testing.T) {
		e := newReceiveEnv(t)
		q := e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		_, before := e.state(t, q, m.ReceiptHandle)
		lost := &countLostPut{Store: e.store}
		require.ErrorIs(t, e.changer(lost).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60)), message.ErrUnavailable)
		assert.Equal(t, 1, lost.puts, "an unknown outcome is never resent")
		_, after := e.state(t, q, m.ReceiptHandle)
		assert.Equal(t, before, after)
	})
	t.Run("state read fails", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		require.ErrorIs(t, e.changer(&failGetState{Store: e.store}).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60)), message.ErrUnavailable)
	})
	for name, value := range map[string]string{
		"unknown state":  `{"v":1,"s":"archived","g":1}`,
		"newer version":  `{"v":2,"s":"inflight","c":1,"g":1,"d":1,"r":1,"f":1}`,
		"moving":         `{"v":1,"s":"moving","c":1,"g":1}`,
		"older than key": `{"v":1,"s":"inflight","c":1,"g":0,"d":1,"r":1,"f":1}`,
		"incomplete":     `{"v":1,"s":"inflight","c":1,"g":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newReceiveEnv(t)
			q := e.create(t, acct, "orders", nil)
			m := e.receiveOne(t, "orders")
			rc, err := receiveKeys.Decode(m.ReceiptHandle, q.Namespace().Queue)
			require.NoError(t, err)
			_, rev := e.state(t, q, m.ReceiptHandle)
			rev, err = e.store.PutState(context.Background(), q.Namespace(), rc.Position, []byte(value), rev)
			require.NoError(t, err)
			err = e.changer(nil).ChangeVisibility(context.Background(), vis("orders", m.ReceiptHandle, 60))
			require.Error(t, err)
			assert.NotErrorIs(t, err, message.ErrNotInflight, "a record not ours is never read as settled")
			st, err := e.store.GetState(context.Background(), q.Namespace(), rc.Position)
			require.NoError(t, err)
			assert.Equal(t, rev, st.Revision, "nothing was written")
		})
	}
}

// deleteFirst runs a delete just before the first state write it sees.
type deleteFirst struct {
	message.Store

	d    *message.Deleter
	in   message.DeleteInput
	done bool
}

func (s *deleteFirst) PutState(ctx context.Context, ns message.Namespace, pos message.Position, v []byte, rev message.Revision) (message.Revision, error) {
	if !s.done {
		s.done = true
		if err := s.d.Delete(ctx, s.in); err != nil {
			return 0, err
		}
	}
	return s.Store.PutState(ctx, ns, pos, v, rev)
}

// countLostPut drops every state write, counting attempts.
type countLostPut struct {
	message.Store

	puts int
}

func (s *countLostPut) PutState(context.Context, message.Namespace, message.Position, []byte, message.Revision) (message.Revision, error) {
	s.puts++
	return 0, errLostAck
}

var errLostAck = errors.New("acknowledgement lost")
