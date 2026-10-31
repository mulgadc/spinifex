package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func markDeleting(f *fixture, name string) (Record, error) {
	return f.store.MarkDeleting(context.Background(), acct, region, name)
}

func TestMarkDeleting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	q, err := f.store.Create(ctx, CreateRequest{Account: acct, Region: region, Name: "orders",
		Attributes: map[string]string{AttrDelaySeconds: "5"}, Tags: map[string]string{"env": "prod"}})
	require.NoError(t, err)
	be, err := f.kv.Get(ctx, bindingKey(acct, region, "orders"))
	require.NoError(t, err)
	f.now = f.now.Add(time.Minute)

	r, err := markDeleting(f, "orders")
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, r.State)

	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, StateDeleting, stored.State)
	assert.Equal(t, uint64(2), stored.Generation)
	require.NotNil(t, stored.Deletion)
	assert.True(t, stored.Deletion.RequestedAt.Equal(f.now))
	assert.Equal(t, uint64(2), stored.Deletion.Generation)
	assert.Equal(t, q.Attributes, stored.Attributes)
	assert.Equal(t, q.Tags, stored.Tags)
	assert.True(t, stored.AttributesModifiedAt.Equal(q.AttributesModifiedAt))

	ae, err := f.kv.Get(ctx, bindingKey(acct, region, "orders"))
	require.NoError(t, err)
	assert.Equal(t, be.Revision(), ae.Revision(), "the binding is not changed by the intent")

	_, err = f.store.Resolve(ctx, acct, region, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist, "publicly absent from the intent write")
	l, err := list(f, ListRequest{})
	require.NoError(t, err)
	assert.Empty(t, l.Queues)
	_, err = set(f, "orders", map[string]string{AttrDelaySeconds: "1"})
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)
}

func TestMarkDeletingNotActive(t *testing.T) {
	f := newFixture(t)
	_, err := markDeleting(f, "missing")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)

	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	_, err = markDeleting(f, "orders")
	require.NoError(t, err)
	_, before := f.record(t, q.QueueID)
	_, err = markDeleting(f, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist, "a deleting queue is already absent")
	_, after := f.record(t, q.QueueID)
	assert.Equal(t, before, after)

	for name, change := range map[string]func(*Record){
		"unknown state":      func(r *Record) { r.State = "migrating" },
		"missing attributes": func(r *Record) { r.Attributes = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			q, err := create(f, "orders", nil)
			require.NoError(t, err)
			f.rewrite(t, q.QueueID, change)
			_, before := f.record(t, q.QueueID)
			_, err = markDeleting(f, "orders")
			assert.ErrorIs(t, err, ErrUnavailable)
			_, after := f.record(t, q.QueueID)
			assert.Equal(t, before, after)
		})
	}
}

// Of concurrent deletes exactly one writes the intent; the rest find the
// queue already absent.
func TestMarkDeletingConcurrent(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = markDeleting(f, "orders") })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		assert.ErrorIs(t, err, ErrQueueDoesNotExist)
	}
	assert.Equal(t, 1, ok)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, uint64(2), stored.Generation)
	assert.Equal(t, uint64(2), stored.Deletion.Generation)
}

func TestMarkDeletingAppliedReplyLost(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	f.fault.onUpdate = func(key string, value []byte, rev uint64) (uint64, error, bool) {
		f.fault.onUpdate = nil
		_, err := f.fault.real.update(context.Background(), key, value, rev)
		require.NoError(t, err)
		return 0, errLostReply, true
	}
	r, err := markDeleting(f, "orders")
	require.NoError(t, err)
	assert.Equal(t, StateDeleting, r.State)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, uint64(2), stored.Generation)
}

func TestMarkDeletingUnknownOutcomeFailsClosed(t *testing.T) {
	cases := map[string]func(f *fixture){
		"record unmoved": func(*fixture) {},
		"re-read fails": func(f *fixture) {
			f.fault.onGet = func(string) ([]byte, uint64, error, bool) {
				f.fault.onGet = nil
				return nil, 0, errLostReply, true
			}
		},
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			q, err := create(f, "orders", nil)
			require.NoError(t, err)
			sends := 0
			f.fault.onUpdate = func(string, []byte, uint64) (uint64, error, bool) {
				sends++
				after(f)
				return 0, errLostReply, true
			}
			_, err = markDeleting(f, "orders")
			assert.ErrorIs(t, err, ErrUnavailable)
			assert.Equal(t, 1, sends)
			stored, _ := f.record(t, q.QueueID)
			assert.Equal(t, StateActive, stored.State)

			f.fault.onUpdate = nil
			_, err = markDeleting(f, "orders")
			require.NoError(t, err)
			stored, _ = f.record(t, q.QueueID)
			assert.Equal(t, uint64(2), stored.Generation)
		})
	}
}
