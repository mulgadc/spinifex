package jetstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errInjectedDelete = errors.New("injected delete failure")

// deleteControl can hold every consumer delete until release is closed and
// fail the next failures of them.
type deleteControl struct {
	jsapi.Stream

	entered  chan struct{}
	release  chan struct{}
	failures atomic.Int32
}

func controlDeletes(f *feed, hold bool) *deleteControl {
	d := &deleteControl{Stream: f.stream, entered: make(chan struct{}, 16)}
	if hold {
		d.release = make(chan struct{})
	}
	f.stream = d
	return d
}

func (d *deleteControl) DeleteConsumer(ctx context.Context, name string) error {
	d.entered <- struct{}{}
	if d.release != nil {
		<-d.release
	}
	if d.failures.Add(-1) >= 0 {
		return errInjectedDelete
	}
	return d.Stream.DeleteConsumer(ctx, name)
}

// returnsWithin reports whether ch delivers within d, and its value.
func returnsWithin(ch <-chan error, d time.Duration) (error, bool) {
	select {
	case err := <-ch:
		return err, true
	case <-time.After(d):
		return nil, false
	}
}

// TestFeedConcurrentCloseWaitsForDelete: a second Close does not report
// success while the first Close's delete is still in progress.
func TestFeedConcurrentCloseWaitsForDelete(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	d := controlDeletes(f, true)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.Close() }()
	<-d.entered
	go func() { second <- f.Close() }()
	_, done := returnsWithin(second, 200*time.Millisecond)
	require.False(t, done, "the second Close returned while the delete was held")
	close(d.release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedCloseWaitsForRecoveryRelease: Close waits for the release of a
// consumer a recovery has detached.
func TestFeedCloseWaitsForRecoveryRelease(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	appendAt(t, s, ns)
	f := openWedgeable(t, s)
	_, err := f.Next(context.Background())
	require.NoError(t, err)
	appendAt(t, s, ns)
	d := controlDeletes(f, true)
	wedge(f)
	next := make(chan error, 1)
	go func() {
		_, err := f.Next(context.Background())
		next <- err
	}()
	<-d.entered
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	_, done := returnsWithin(closed, 200*time.Millisecond)
	require.False(t, done, "Close returned while the recovery's delete was held")
	close(d.release)
	require.NoError(t, <-closed)
	assert.Empty(t, consumerNames(t, s))
	require.ErrorIs(t, <-next, message.ErrFeedClosed)
}

// TestFeedCloseRetriesFailedDelete: a failed delete is an error, not success,
// and a later Close retries it.
func TestFeedCloseRetriesFailedDelete(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	d := controlDeletes(f, false)
	d.failures.Store(2)
	require.ErrorIs(t, f.Close(), errInjectedDelete, "the release and Close's own retry both fail")
	require.Len(t, consumerNames(t, s), 1, "the consumer is still on the server")
	require.NoError(t, f.Close(), "a later Close retries the delete")
	assert.Empty(t, consumerNames(t, s))
	assert.NoError(t, f.Close())
}

// TestFeedCloseRetriesFailedRecoveryRelease: a recovery's failed delete is
// retried by Close rather than forgotten.
func TestFeedCloseRetriesFailedRecoveryRelease(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	appendAt(t, s, ns)
	f := openWedgeable(t, s)
	_, err := f.Next(context.Background())
	require.NoError(t, err)
	appendAt(t, s, ns)
	d := controlDeletes(f, false)
	d.failures.Store(1)
	wedge(f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, errInjectedDelete)
	require.NoError(t, f.Close())
	assert.Empty(t, consumerNames(t, s), "the abandoned consumer was deleted by Close")
}

// TestFeedDeliveryClaimedBeforeCloseIsReturned: Close beginning after Next has
// claimed a change does not withdraw it, and the feed then reports closed.
func TestFeedDeliveryClaimedBeforeCloseIsReturned(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	first := appendAt(t, s, ns)
	second := appendAt(t, s, ns)
	f := openWedgeable(t, s)
	f.claimed = func() {
		f.claimed = nil
		require.NoError(t, f.Close())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := f.Next(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, c.Position, "the change claimed before Close is delivered")
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, message.ErrFeedClosed)

	resumed, err := s.Feed(ctx, 0, c.Cursor)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	got, err := resumed.Next(ctx)
	require.NoError(t, err)
	assert.Equal(t, second, got.Position, "resuming after it neither repeats nor skips")
}

// lostReply creates the consumer but reports the create as failed, as a
// cancelled or timed-out request does after the server has acted on it.
type lostReply struct{ jsapi.Stream }

func (l lostReply) OrderedConsumer(_ context.Context, cfg jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	if _, err := l.Stream.OrderedConsumer(context.Background(), cfg); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

func TestFeedFailedCreateLeavesNoConsumer(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	require.NoError(t, f.discard(f.detachForTest()))
	f.stream = lostReply{f.stream}
	_, _, err := f.open(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, consumerNames(t, s), "the create that landed is deleted by name")
}

// TestFeedCallerDeadlineDoesNotCutCreateShort: real creates under caller
// deadlines that expire mid-request complete, so none lands unowned.
func TestFeedCallerDeadlineDoesNotCutCreateShort(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	require.NoError(t, f.discard(f.detachForTest()))
	for i := range 100 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%40)*25*time.Microsecond)
		consumer, it, err := f.open(ctx)
		cancel()
		require.NoError(t, err, "create %d", i)
		require.NoError(t, release(f.stream, consumer, it))
	}
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedCloseRetriesAbandonedCreate: when the delete after a failed create
// finds nothing, Close deletes the name again, catching a create that landed
// after that first delete.
func TestFeedCloseRetriesAbandonedCreate(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	require.NoError(t, f.discard(f.detachForTest()))
	late := &lateCreate{Stream: f.stream}
	f.stream = late
	_, _, err := f.open(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, late.land())
	require.Len(t, consumerNames(t, s), 1, "the late create is on the server")
	require.NoError(t, f.Close())
	assert.Empty(t, consumerNames(t, s))
}

// lateCreate reports a create as timed out before making it; land then makes
// it, as a request the server acts on after the client gave up would.
type lateCreate struct {
	jsapi.Stream

	cfg jsapi.OrderedConsumerConfig
}

func (l *lateCreate) OrderedConsumer(_ context.Context, cfg jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	l.cfg = cfg
	return nil, context.DeadlineExceeded
}

func (l *lateCreate) land() error {
	_, err := l.Stream.OrderedConsumer(context.Background(), l.cfg)
	return err
}
