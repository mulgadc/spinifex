package jetstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e receiveEnv) deleter(store message.Store) *message.Deleter {
	if store == nil {
		store = e.store
	}
	return &message.Deleter{Store: store, Queues: e.queues, Keys: receiveKeys}
}

func del(name, handle string) message.DeleteInput {
	return message.DeleteInput{Account: acct, Region: region, Queue: name, ReceiptHandle: handle}
}

// receiveOne sends a message and receives it, returning the received copy.
func (e receiveEnv) receiveOne(t *testing.T, name string) message.Received {
	t.Helper()
	e.send(t, name, "hello", nil)
	got, err := e.receiver(nil).Receive(context.Background(), recv(name, 1))
	require.NoError(t, err)
	require.Len(t, got, 1)
	return got[0]
}

func (e receiveEnv) state(t *testing.T, q queue.Record, h string) (map[string]any, message.Revision) {
	t.Helper()
	rc, err := receiveKeys.Decode(h, q.Namespace().Queue)
	require.NoError(t, err)
	st, err := e.store.GetState(context.Background(), q.Namespace(), rc.Position)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(st.Value, &m))
	return m, st.Revision
}

func TestDeleteCurrentHandleWritesTombstone(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	m := e.receiveOne(t, "orders")

	require.NoError(t, e.deleter(nil).Delete(context.Background(), del("orders", m.ReceiptHandle)))
	st, _ := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, message.StatusDeleted, st["s"])
	assert.EqualValues(t, 1, st["g"], "the tombstone keeps the generation")

	e.clock.advance(time.Hour)
	got, err := e.receiver(nil).Receive(context.Background(), recv("orders", 10))
	require.NoError(t, err)
	assert.Empty(t, got, "a deleted message is never received again")

	rc, err := receiveKeys.Decode(m.ReceiptHandle, q.Namespace().Queue)
	require.NoError(t, err)
	body, err := e.store.Get(context.Background(), q.Namespace(), rc.Position)
	require.NoError(t, err, "the body is left for the sweeper")
	assert.Equal(t, []byte("hello"), body.Body)
}

// An older handle succeeds without deleting the newer receive.
func TestDeleteOlderHandleKeepsNewerReceive(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	first := e.receiveOne(t, "orders")
	e.clock.advance(time.Hour)
	again, err := e.receiver(nil).Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, again, 1)
	_, before := e.state(t, q, first.ReceiptHandle)

	require.NoError(t, e.deleter(nil).Delete(context.Background(), del("orders", first.ReceiptHandle)))
	st, after := e.state(t, q, first.ReceiptHandle)
	assert.Equal(t, message.StatusInflight, st["s"])
	assert.EqualValues(t, 2, st["g"])
	assert.Equal(t, before, after, "nothing was written")

	require.NoError(t, e.deleter(nil).Delete(context.Background(), del("orders", again[0].ReceiptHandle)))
	st, _ = e.state(t, q, first.ReceiptHandle)
	assert.Equal(t, message.StatusDeleted, st["s"], "the newest handle deletes")
}

func TestDeleteIsIdempotent(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	m := e.receiveOne(t, "orders")
	d := e.deleter(nil)
	require.NoError(t, d.Delete(context.Background(), del("orders", m.ReceiptHandle)))
	_, rev := e.state(t, q, m.ReceiptHandle)
	require.NoError(t, d.Delete(context.Background(), del("orders", m.ReceiptHandle)))
	_, again := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, rev, again, "a second delete writes nothing")

	rc, err := receiveKeys.Decode(m.ReceiptHandle, q.Namespace().Queue)
	require.NoError(t, err)
	require.NoError(t, e.store.DeleteState(context.Background(), q.Namespace(), rc.Position, rev))
	require.NoError(t, d.Delete(context.Background(), del("orders", m.ReceiptHandle)), "a record already gone succeeds")
}

// A delete racing a re-receive either tombstones before it, so the receive
// gets nothing, or loses to it and leaves the newer receive in flight.
func TestDeleteRacingReReceive(t *testing.T) {
	for range 10 {
		e := newReceiveEnv(t)
		q := e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		e.clock.advance(time.Hour)
		var (
			wg   sync.WaitGroup
			got  []message.Received
			rerr error
		)
		wg.Go(func() { got, rerr = e.receiver(nil).Receive(context.Background(), recv("orders", 1)) })
		derr := e.deleter(nil).Delete(context.Background(), del("orders", m.ReceiptHandle))
		wg.Wait()
		require.NoError(t, derr)
		require.NoError(t, rerr)
		st, _ := e.state(t, q, m.ReceiptHandle)
		if len(got) == 1 {
			assert.Equal(t, message.StatusInflight, st["s"], "the newer receive was not deleted")
			assert.EqualValues(t, 2, st["g"])
		} else {
			assert.Equal(t, message.StatusDeleted, st["s"])
		}
	}
}

func TestDeleteRefusesInvalidHandles(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	e.create(t, acct, "audit", nil)
	m := e.receiveOne(t, "orders")
	forger, err := message.NewReceiptKeys(9, map[uint32][]byte{9: bytes.Repeat([]byte{3}, message.MinReceiptKeyLen)})
	require.NoError(t, err)
	rc, err := receiveKeys.Decode(m.ReceiptHandle, q.Namespace().Queue)
	require.NoError(t, err)
	forged, err := forger.Encode(rc)
	require.NoError(t, err)
	_, before := e.state(t, q, m.ReceiptHandle)

	for name, in := range map[string]message.DeleteInput{
		"malformed":         del("orders", "not-a-handle"),
		"empty":             del("orders", ""),
		"tampered":          del("orders", m.ReceiptHandle[:len(m.ReceiptHandle)-2]+"AA"),
		"unknown key":       del("orders", forged),
		"another queue":     del("audit", m.ReceiptHandle),
		"another account's": {Account: other, Region: region, Queue: "orders", ReceiptHandle: m.ReceiptHandle},
	} {
		err := e.deleter(nil).Delete(context.Background(), in)
		require.Error(t, err, name)
		if name != "another account's" {
			require.ErrorIs(t, err, message.ErrReceiptInvalid, name)
		}
	}
	_, after := e.state(t, q, m.ReceiptHandle)
	assert.Equal(t, before, after, "no refused handle wrote anything")
}

func TestDeleteUnsettledOutcomes(t *testing.T) {
	t.Run("lost reply, tombstone applied", func(t *testing.T) {
		e := newReceiveEnv(t)
		q := e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		require.NoError(t, e.deleter(&receiveFaults{Store: e.store, putFails: 1}).Delete(context.Background(), del("orders", m.ReceiptHandle)),
			"a tombstone in place settles the lost reply")
		st, _ := e.state(t, q, m.ReceiptHandle)
		assert.Equal(t, message.StatusDeleted, st["s"])
	})
	t.Run("lost reply, nothing applied", func(t *testing.T) {
		e := newReceiveEnv(t)
		q := e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		_, before := e.state(t, q, m.ReceiptHandle)
		err := e.deleter(&lostPut{Store: e.store}).Delete(context.Background(), del("orders", m.ReceiptHandle))
		require.ErrorIs(t, err, message.ErrUnavailable, "the write may still land")
		_, after := e.state(t, q, m.ReceiptHandle)
		assert.Equal(t, before, after)
	})
	t.Run("state read fails", func(t *testing.T) {
		e := newReceiveEnv(t)
		e.create(t, acct, "orders", nil)
		m := e.receiveOne(t, "orders")
		err := e.deleter(&failGetState{Store: e.store}).Delete(context.Background(), del("orders", m.ReceiptHandle))
		require.ErrorIs(t, err, message.ErrUnavailable)
	})
	for name, value := range map[string]string{
		"unknown state":  `{"v":1,"s":"archived","g":1}`,
		"newer version":  `{"v":2,"s":"inflight","c":1,"g":1,"d":1,"r":1,"f":1}`,
		"moving":         `{"v":1,"s":"moving","c":1,"g":1}`,
		"older than key": `{"v":1,"s":"inflight","c":1,"g":0,"d":1,"r":1,"f":1}`,
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
			err = e.deleter(nil).Delete(context.Background(), del("orders", m.ReceiptHandle))
			require.Error(t, err, "never reported as deleted")
			st, err := e.store.GetState(context.Background(), q.Namespace(), rc.Position)
			require.NoError(t, err)
			assert.Equal(t, rev, st.Revision, "nothing was written")
		})
	}
}

// Neither handles nor bodies appear in errors or logs on any refusal.
func TestDeleteKeepsHandlesOutOfErrorsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e := newReceiveEnv(t)
	e.create(t, acct, "orders", nil)
	e.create(t, acct, "audit", nil)
	m := e.receiveOne(t, "orders")
	var errs []string
	for _, in := range []message.DeleteInput{
		del("audit", m.ReceiptHandle),
		del("orders", m.ReceiptHandle[:len(m.ReceiptHandle)-2]+"AA"),
	} {
		if err := e.deleter(&lostPut{Store: e.store}).Delete(context.Background(), in); err != nil {
			errs = append(errs, err.Error())
		}
	}
	errs = append(errs, e.deleter(&lostPut{Store: e.store}).Delete(context.Background(), del("orders", m.ReceiptHandle)).Error())
	all := strings.Join(errs, "\n") + logs.String()
	assert.NotContains(t, all, m.ReceiptHandle[:20])
	assert.NotContains(t, all, "hello")
}

// lostPut drops every state write and reports a lost acknowledgement.
type lostPut struct{ message.Store }

func (lostPut) PutState(context.Context, message.Namespace, message.Position, []byte, message.Revision) (message.Revision, error) {
	return 0, errors.New("acknowledgement lost")
}

type failGetState struct{ message.Store }

func (failGetState) GetState(context.Context, message.Namespace, message.Position) (message.State, error) {
	return message.State{}, errors.New("timeout")
}
