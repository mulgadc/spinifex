package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noCreate fails the test if a consumer create is attempted.
type noCreate struct {
	jsapi.Stream

	t *testing.T
}

func (n noCreate) OrderedConsumer(context.Context, jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	n.t.Error("a consumer create was started after ctx ended")
	return nil, context.Canceled
}

// emptied leaves f with no consumer, as recovery's detach does.
func emptied(t *testing.T, f *feed) {
	t.Helper()
	require.NoError(t, f.discard(f.detachForTest()))
}

// TestFeedCancelledBeforeOpenCreatesNothing: Feed with an ended ctx returns
// its error and creates no consumer.
func TestFeedCancelledBeforeOpenCreatesNothing(t *testing.T) {
	s, _ := feedStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Feed(ctx, 0, 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, consumerNames(t, s))

	f := openWedgeable(t, s)
	emptied(t, f)
	f.stream = noCreate{Stream: f.stream, t: t}
	require.ErrorIs(t, f.start(ctx), context.Canceled)
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, context.Canceled, "recovery does not start an open after ctx ended")
}

// TestFeedStartCancelledDuringCreate: a first open whose create finishes after
// ctx ended releases that consumer and returns ctx's error.
func TestFeedStartCancelledDuringCreate(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	emptied(t, f)
	b := &blockingStream{
		Stream:  f.stream,
		entered: make(chan struct{}), release: make(chan struct{}),
		created: make(chan struct{}), proceed: make(chan struct{}),
	}
	f.stream = b
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.start(ctx) }()
	<-b.entered
	close(b.release)
	<-b.created
	require.Len(t, consumerNames(t, s), 1, "the create landed")
	cancel()
	close(b.proceed)
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Nil(t, f.it, "the late consumer is not kept")
	assert.Empty(t, consumerNames(t, s), "the late consumer is deleted")
	assert.Zero(t, f.busy)
}

// TestFeedRecoveryCancelledDuringCreate: a recovery whose create finishes
// after ctx ended releases that consumer, returns ctx's error and no change;
// the stored change is delivered by the next Next.
func TestFeedRecoveryCancelledDuringCreate(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	b, next := recoverInto(ctx, t, s, f)
	close(b.release)
	<-b.created
	cancel()
	close(b.proceed)
	require.ErrorIs(t, <-next, context.Canceled)
	assert.Empty(t, consumerNames(t, s), "the late consumer is deleted")

	f.stream = b.Stream
	c, err := f.Next(context.Background())
	require.NoError(t, err)
	assert.Equal(t, message.Position(2), c.Position, "the withheld change is not lost")
	require.NoError(t, f.Close())
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedRecoveryCloseWinsOverCancel: when Close has begun and ctx has ended
// during a recovery's create, Next returns ErrFeedClosed and Close succeeds
// only once the late consumer is deleted.
func TestFeedRecoveryCloseWinsOverCancel(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	b, next := recoverInto(ctx, t, s, f)
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	require.Eventually(t, f.isClosed, 5*time.Second, 5*time.Millisecond)
	cancel()
	close(b.release)
	close(b.proceed)
	require.ErrorIs(t, <-next, message.ErrFeedClosed)
	require.NoError(t, <-closed)
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedConsumerHasInactiveBackstop: every feed consumer carries the
// server-side inactive threshold that removes one an ambiguous create left.
func TestFeedConsumerHasInactiveBackstop(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	info, err := f.consumer.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, time.Minute, info.Config.InactiveThreshold)
}

var errInjectedCreate = errors.New("injected create failure")

// cancelThenFail ends the caller's ctx during a create, then fails it.
type cancelThenFail struct {
	jsapi.Stream

	cancel context.CancelFunc
}

func (c cancelThenFail) OrderedConsumer(context.Context, jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	c.cancel()
	return nil, errInjectedCreate
}

// TestFeedFailedOpenAfterCancelReportsCancellation: an open that fails after
// ctx ended still reports ctx's error, alongside the create failure.
func TestFeedFailedOpenAfterCancelReportsCancellation(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	emptied(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	f.stream = cancelThenFail{Stream: f.stream, cancel: cancel}
	err := f.start(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errInjectedCreate)
	assert.Nil(t, f.it)
}
