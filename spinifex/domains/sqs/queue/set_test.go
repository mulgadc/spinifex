package queue

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func set(f *fixture, name string, attrs map[string]string) (Record, error) {
	return f.store.SetAttributes(context.Background(), acct, region, name, attrs)
}

func (f *fixture) record(t *testing.T, queueID string) (Record, uint64) {
	t.Helper()
	e, err := f.kv.Get(context.Background(), recordKey(acct, queueID))
	require.NoError(t, err)
	r, err := DecodeRecord(e.Value())
	require.NoError(t, err)
	return r, e.Revision()
}

func (f *fixture) rewrite(t *testing.T, queueID string, change func(*Record)) {
	t.Helper()
	r, rev := f.record(t, queueID)
	change(&r)
	enc, err := r.Encode()
	require.NoError(t, err)
	_, err = f.kv.Update(context.Background(), recordKey(acct, queueID), enc, rev)
	require.NoError(t, err)
}

func TestSetAttributes(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	require.NoError(t, err)
	f.now = f.now.Add(time.Hour)

	r, err := set(f, "orders", map[string]string{AttrVisibilityTimeout: "120", AttrMessageRetentionPeriod: "60"})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), r.Generation)
	assert.True(t, r.CreatedAt.Equal(q.CreatedAt))
	assert.True(t, r.AttributesModifiedAt.Equal(f.now))

	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, "120", stored.Attributes[AttrVisibilityTimeout])
	assert.Equal(t, "60", stored.Attributes[AttrMessageRetentionPeriod])
	assert.Equal(t, "5", stored.Attributes[AttrDelaySeconds], "unnamed attributes keep their values")
	assert.Equal(t, uint64(2), stored.Generation)
}

func TestSetAttributesAllOrNothing(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	_, before := f.record(t, q.QueueID)

	for _, attrs := range []map[string]string{
		{AttrDelaySeconds: "5", AttrVisibilityTimeout: "43201"},
		{AttrDelaySeconds: "5", "Bogus": "1"},
		{AttrDelaySeconds: "5", "RedrivePolicy": "{}"},
		{AttrDelaySeconds: "5", AttrFifoQueue: "false"},
	} {
		_, err := set(f, "orders", attrs)
		assert.Error(t, err, "%v", attrs)
	}
	stored, after := f.record(t, q.QueueID)
	assert.Equal(t, before, after, "a rejected request writes nothing")
	assert.Equal(t, "0", stored.Attributes[AttrDelaySeconds])
}

// Concurrent updates of different attributes each apply exactly once.
func TestSetAttributesConcurrent(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	updates := []map[string]string{
		{AttrDelaySeconds: "9"},
		{AttrVisibilityTimeout: "99"},
		{AttrMessageRetentionPeriod: "999"},
		{AttrMaximumMessageSize: "9999"},
		{AttrReceiveMessageWaitTimeSeconds: "9"},
	}
	errs := make([]error, len(updates))
	var wg sync.WaitGroup
	for i, u := range updates {
		wg.Go(func() { _, errs[i] = set(f, "orders", u) })
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	r, _ := f.record(t, q.QueueID)
	for _, u := range updates {
		for k, v := range u {
			assert.Equal(t, v, r.Attributes[k], k)
		}
	}
	assert.Equal(t, uint64(1+len(updates)), r.Generation)
}

// A DeleteQueue that commits between the read and the update wins: the
// update's compare-and-swap is refused and the queue is reported absent.
func TestSetAttributesLosesToDeletion(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	f.fault.onUpdate = func(key string, value []byte, rev uint64) (uint64, error, bool) {
		if !strings.HasPrefix(key, "q.") {
			return 0, nil, false
		}
		f.fault.onUpdate = nil
		f.rewrite(t, q.QueueID, func(r *Record) {
			r.State = StateDeleting
			r.Generation++
			r.Deletion = &Deletion{RequestedAt: f.now, Generation: r.Generation}
		})
		n, err := f.fault.real.update(context.Background(), key, value, rev)
		return n, err, true
	}
	_, err = set(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)
	r, _ := f.record(t, q.QueueID)
	assert.Equal(t, StateDeleting, r.State)
	assert.Equal(t, "0", r.Attributes[AttrDelaySeconds])
	assert.Equal(t, uint64(2), r.Generation)
}

// A lost reply is not success even when the write applied: the request
// cannot prove its own outcome, so it fails closed and is not resent.
func TestSetAttributesAppliedReplyLost(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	sends := 0
	f.fault.onUpdate = func(key string, value []byte, rev uint64) (uint64, error, bool) {
		sends++
		_, err := f.fault.real.update(context.Background(), key, value, rev)
		require.NoError(t, err)
		return 0, errLostReply, true
	}
	_, err = set(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, 1, sends)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, uint64(2), stored.Generation, "the update applied once")
}

// An update with no reply against a record that has not moved may still
// apply, so the request fails closed; a retry applies it once.
func TestSetAttributesUnknownOutcomeFailsClosed(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	sends := 0
	f.fault.onUpdate = func(string, []byte, uint64) (uint64, error, bool) {
		sends++
		return 0, errLostReply, true
	}
	_, err = set(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, 1, sends, "an unknown outcome is not resent in the same request")
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, uint64(1), stored.Generation)

	f.fault.onUpdate = nil
	_, err = set(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	require.NoError(t, err)
	stored, _ = f.record(t, q.QueueID)
	assert.Equal(t, uint64(2), stored.Generation)
	assert.Equal(t, "5", stored.Attributes[AttrDelaySeconds])
}
