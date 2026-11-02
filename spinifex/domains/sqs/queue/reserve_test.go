package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reserveName(f *fixture, queueID string) error {
	return f.store.Reserve(context.Background(), acct, queueID)
}

func (f *fixture) binding(t *testing.T, name string) (Binding, uint64) {
	t.Helper()
	e, err := f.kv.Get(context.Background(), bindingKey(acct, region, name))
	require.NoError(t, err)
	b, err := DecodeBinding(e.Value())
	require.NoError(t, err)
	return b, e.Revision()
}

// deleted creates a queue and records its deletion intent.
func (f *fixture) deleted(t *testing.T, name string) Record {
	t.Helper()
	_, err := create(f, name, nil)
	require.NoError(t, err)
	r, err := markDeleting(f, name)
	require.NoError(t, err)
	return r
}

func TestReserve(t *testing.T) {
	f := newFixture(t)
	a := f.deleted(t, "orders")
	_, rrev := f.record(t, a.QueueID)

	require.NoError(t, reserveName(f, a.QueueID))
	b, brev := f.binding(t, "orders")
	assert.Equal(t, BindingReserved, b.State)
	assert.Equal(t, a.QueueID, b.QueueID)
	assert.True(t, b.ReuseAfter.Equal(a.Deletion.RequestedAt.Add(ReservationPeriod)))
	_, after := f.record(t, a.QueueID)
	assert.Equal(t, rrev, after, "the queue record is not written")

	require.NoError(t, reserveName(f, a.QueueID), "reserving again is a no-op")
	_, again := f.binding(t, "orders")
	assert.Equal(t, brev, again)
}

func TestReservedNameReuse(t *testing.T) {
	f := newFixture(t)
	a := f.deleted(t, "orders")
	require.NoError(t, reserveName(f, a.QueueID))

	f.now = a.Deletion.RequestedAt.Add(ReservationPeriod - 1)
	_, err := create(f, "orders", nil)
	assert.ErrorIs(t, err, ErrQueueDeletedRecently)

	f.now = a.Deletion.RequestedAt.Add(ReservationPeriod)
	b, err := create(f, "orders", nil)
	require.NoError(t, err)
	assert.NotEqual(t, a.QueueID, b.QueueID)
	stored, _ := f.record(t, a.QueueID)
	assert.Equal(t, StateDeleting, stored.State, "the deleted queue's record remains")
}

// Reserve and CreateQueue race on the same binding revision: one wins, and
// the name is never bound to a replacement before the deadline.
func TestReserveRacesCreate(t *testing.T) {
	for _, lapsed := range []bool{false, true} {
		t.Run(fmt.Sprintf("lapsed=%v", lapsed), func(t *testing.T) {
			for range 10 {
				f := newFixture(t)
				a := f.deleted(t, "orders")
				if lapsed {
					f.now = a.Deletion.RequestedAt.Add(ReservationPeriod)
				}
				var rerr, cerr error
				var b Record
				var wg sync.WaitGroup
				wg.Go(func() { rerr = reserveName(f, a.QueueID) })
				wg.Go(func() { b, cerr = create(f, "orders", nil) })
				wg.Wait()
				require.NoError(t, rerr)
				got, _ := f.binding(t, "orders")
				if !lapsed {
					assert.ErrorIs(t, cerr, ErrQueueDeletedRecently)
					assert.Equal(t, a.QueueID, got.QueueID)
					assert.Equal(t, BindingReserved, got.State)
					continue
				}
				require.NoError(t, cerr)
				assert.Equal(t, b.QueueID, got.QueueID)
				assert.Equal(t, BindingBound, got.State)
			}
		})
	}
}

func TestReserveLostReply(t *testing.T) {
	cases := map[string]struct {
		after   func(t *testing.T, f *fixture, key string, value []byte, rev uint64)
		wantErr error
	}{
		"applied": {
			after: func(t *testing.T, f *fixture, key string, value []byte, rev uint64) {
				_, err := f.fault.real.update(context.Background(), key, value, rev)
				require.NoError(t, err)
			},
		},
		"unmoved": {after: func(*testing.T, *fixture, string, []byte, uint64) {}, wantErr: ErrUnavailable},
		"re-read fails": {
			after: func(_ *testing.T, f *fixture, _ string, _ []byte, _ uint64) {
				f.fault.onGet = func(string) ([]byte, uint64, error, bool) {
					f.fault.onGet = nil
					return nil, 0, errLostReply, true
				}
			},
			wantErr: ErrUnavailable,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := f.deleted(t, "orders")
			sends := 0
			f.fault.onUpdate = func(key string, value []byte, rev uint64) (uint64, error, bool) {
				sends++
				tc.after(t, f, key, value, rev)
				return 0, errLostReply, true
			}
			err := reserveName(f, a.QueueID)
			f.fault.onUpdate = nil
			assert.Equal(t, 1, sends)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				b, _ := f.binding(t, "orders")
				assert.Equal(t, BindingBound, b.State)
				return
			}
			require.NoError(t, err)
			b, _ := f.binding(t, "orders")
			assert.Equal(t, BindingReserved, b.State)
		})
	}
}
