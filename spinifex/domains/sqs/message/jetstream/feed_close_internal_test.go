package jetstream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingStream holds a consumer create until release, then holds its
// result, with the consumer already on the server, until proceed.
type blockingStream struct {
	jsapi.Stream

	entered, release, created, proceed chan struct{}
	deleteErr                          error
}

func (b *blockingStream) OrderedConsumer(ctx context.Context, cfg jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	close(b.entered)
	<-b.release
	c, err := b.Stream.OrderedConsumer(ctx, cfg)
	close(b.created)
	<-b.proceed
	return c, err
}

func (b *blockingStream) DeleteConsumer(ctx context.Context, name string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	return b.Stream.DeleteConsumer(ctx, name)
}

// recoverInto wedges f with a change stored after its cursor and starts a
// Next under ctx that reopens through a blockingStream; it returns once the
// open blocks.
func recoverInto(ctx context.Context, t *testing.T, s *Store, f *feed) (*blockingStream, chan error) {
	t.Helper()
	ns := message.Namespace{Queue: "q"}
	appendAt(t, s, ns)
	_, err := f.Next(context.Background())
	require.NoError(t, err)
	appendAt(t, s, ns)
	b := &blockingStream{
		Stream:  f.stream,
		entered: make(chan struct{}), release: make(chan struct{}),
		created: make(chan struct{}), proceed: make(chan struct{}),
	}
	f.stream = b
	wedge(f)
	done := make(chan error, 1)
	go func() {
		_, err := f.Next(ctx)
		done <- err
	}()
	<-b.entered
	return b, done
}

func TestFeedNextAfterCloseCreatesNoConsumer(t *testing.T) {
	s, _ := feedStore(t)
	appendAt(t, s, message.Namespace{Queue: "q"})
	f := openWedgeable(t, s)
	require.NoError(t, f.Close())
	require.Empty(t, consumerNames(t, s))

	_, err := f.Next(context.Background())
	require.ErrorIs(t, err, message.ErrFeedClosed)
	assert.Empty(t, consumerNames(t, s), "Next after Close opens nothing")
	assert.NoError(t, f.Close(), "a repeated Close is a no-op")
	assert.NoError(t, f.Close())
}

// TestFeedCloseInterruptsBlockedNext: a Next waiting on a consumer with
// nothing to deliver returns message.ErrFeedClosed promptly once Close runs.
func TestFeedCloseInterruptsBlockedNext(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	f.idleCheck = time.Minute
	done := make(chan error, 1)
	go func() {
		_, err := f.Next(context.Background())
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, f.Close())
	select {
	case err := <-done:
		require.ErrorIs(t, err, message.ErrFeedClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Next still blocked after Close")
	}
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedCloseConcurrentWithNext races Close against active Next calls and
// recoveries; run under -race. Every round ends closed with no consumer.
func TestFeedCloseConcurrentWithNext(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	for range 3 {
		appendAt(t, s, ns)
	}
	for round := range 20 {
		f := openWedgeable(t, s)
		f.idleCheck = 20 * time.Millisecond
		var wg sync.WaitGroup
		var last error
		wg.Go(func() {
			for {
				_, err := f.Next(context.Background())
				if err != nil {
					last = err
					return
				}
			}
		})
		time.Sleep(time.Duration(round%5) * 15 * time.Millisecond)
		var closers sync.WaitGroup
		for range 3 {
			closers.Go(func() { assert.NoError(t, f.Close()) })
		}
		closers.Wait()
		wg.Wait()
		require.ErrorIs(t, last, message.ErrFeedClosed, "round %d", round)
		require.Eventually(t, func() bool { return len(consumerNames(t, s)) == 0 }, 5*time.Second, 20*time.Millisecond, "round %d", round)
	}
}

// stallingStream holds every message get until its context ends, as a slow
// or unreachable leader would.
type stallingStream struct {
	jsapi.Stream

	entered chan struct{}
}

func (s *stallingStream) GetMsg(ctx context.Context, _ uint64, _ ...jsapi.GetMsgOpt) (*jsapi.RawStreamMsg, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestFeedCloseInterruptsRecoveryProbe: Close ends a Next blocked in the
// idle check's stream read, not only one waiting on the consumer.
func TestFeedCloseInterruptsRecoveryProbe(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	stall := &stallingStream{Stream: f.stream, entered: make(chan struct{})}
	f.stream = stall
	done := make(chan error, 1)
	go func() {
		_, err := f.Next(context.Background())
		done <- err
	}()
	<-stall.entered
	// Remove the iterator first, so only cancellation can end the read.
	consumer, it := f.detach()
	require.NoError(t, f.discard(consumer, it))
	require.NoError(t, f.Close())
	select {
	case err := <-done:
		require.ErrorIs(t, err, message.ErrFeedClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Next still blocked in the idle check after Close")
	}
}

// TestFeedCloseWaitsForInFlightOpen: Close waits for a reopen already
// creating a consumer, and that consumer is gone when Close returns nil.
func TestFeedCloseWaitsForInFlightOpen(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	b, next := recoverInto(context.Background(), t, s, f)
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a consumer create was still in progress")
	case <-time.After(200 * time.Millisecond):
	}
	close(b.release)
	close(b.proceed)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the open finished")
	}
	assert.Empty(t, consumerNames(t, s), "no consumer survives a successful Close")
	require.ErrorIs(t, <-next, message.ErrFeedClosed)
}

// TestFeedCloseReportsStuckOpen: past closeWait, Close returns an error
// rather than success; a repeated Close waits again and succeeds once the
// late consumer has been deleted.
func TestFeedCloseReportsStuckOpen(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	f.closeWait = 200 * time.Millisecond
	b, next := recoverInto(context.Background(), t, s, f)
	err := f.Close()
	require.ErrorIs(t, err, errCleanupPending)
	close(b.release)
	<-b.created
	f.closeWait = 5 * time.Second
	again := make(chan error, 1)
	go func() { again <- f.Close() }()
	select {
	case <-again:
		t.Fatal("a repeated Close returned while the late consumer still existed")
	case <-time.After(200 * time.Millisecond):
	}
	close(b.proceed)
	require.NoError(t, <-again)
	assert.Empty(t, consumerNames(t, s))
	require.ErrorIs(t, <-next, message.ErrFeedClosed)
}

// TestFeedCloseReportsLateDeleteFailure: if the consumer a racing reopen
// created cannot be deleted, Close returns that error, not success.
func TestFeedCloseReportsLateDeleteFailure(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	b, next := recoverInto(context.Background(), t, s, f)
	injected := errors.New("injected delete failure")
	b.deleteErr = injected
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	for !f.isClosed() {
		time.Sleep(time.Millisecond)
	}
	close(b.release)
	close(b.proceed)
	require.ErrorIs(t, <-closed, injected)
	require.ErrorIs(t, <-next, message.ErrFeedClosed)
}

// closeOnDelivery takes a real delivery, then lets Close begin before Next
// hands it back.
type closeOnDelivery struct {
	jsapi.MessagesContext

	f *feed
}

func (c *closeOnDelivery) Next(opts ...jsapi.NextOpt) (jsapi.Msg, error) {
	msg, err := c.MessagesContext.Next(opts...)
	if err == nil {
		go func() { _ = c.f.Close() }()
		for !c.f.isClosed() {
			time.Sleep(time.Millisecond)
		}
	}
	return msg, err
}

// TestFeedWithholdsDeliveryRacingClose: a change received after Close began
// is not returned, and a feed from the last delivered cursor still gets it.
func TestFeedWithholdsDeliveryRacingClose(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	first := appendAt(t, s, ns)
	second := appendAt(t, s, ns)
	f := openWedgeable(t, s)
	c, err := f.Next(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, c.Position)
	f.it = &closeOnDelivery{MessagesContext: f.it, f: f}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, message.ErrFeedClosed)

	resumed, err := s.Feed(ctx, 0, c.Cursor)
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	got, err := resumed.Next(ctx)
	require.NoError(t, err)
	assert.Equal(t, second, got.Position, "the withheld change is not lost")
}
