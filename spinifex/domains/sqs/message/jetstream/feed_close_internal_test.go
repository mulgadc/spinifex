package jetstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closingStream runs beforeOpen ahead of every consumer it creates.
type closingStream struct {
	jsapi.Stream

	beforeOpen func()
}

func (c *closingStream) OrderedConsumer(ctx context.Context, cfg jsapi.OrderedConsumerConfig) (jsapi.Consumer, error) {
	c.beforeOpen()
	return c.Stream.OrderedConsumer(ctx, cfg)
}

func TestFeedNextAfterCloseCreatesNoConsumer(t *testing.T) {
	s, _ := feedStore(t)
	appendAt(t, s, message.Namespace{Queue: "q"})
	f := openWedgeable(t, s)
	require.NoError(t, f.Close())
	require.Empty(t, consumerNames(t, s))

	_, err := f.Next(context.Background())
	require.ErrorIs(t, err, ErrFeedClosed)
	assert.Empty(t, consumerNames(t, s), "Next after Close opens nothing")
	assert.NoError(t, f.Close(), "a repeated Close is a no-op")
	assert.NoError(t, f.Close())
}

// TestFeedCloseInterruptsBlockedNext: a Next waiting on a consumer with
// nothing to deliver returns ErrFeedClosed promptly once Close runs.
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
		require.ErrorIs(t, err, ErrFeedClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Next still blocked after Close")
	}
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedCloseDuringRecoveryReleasesReplacement: when Close wins the race
// with a recovery's reopen, the replacement consumer is deleted, not adopted.
func TestFeedCloseDuringRecoveryReleasesReplacement(t *testing.T) {
	s, _ := feedStore(t)
	ns := message.Namespace{Queue: "q"}
	appendAt(t, s, ns)
	appendAt(t, s, ns)
	f := openWedgeable(t, s)
	_, err := f.Next(context.Background())
	require.NoError(t, err)
	opens := 0
	f.stream = &closingStream{Stream: f.stream, beforeOpen: func() {
		opens++
		require.NoError(t, f.Close())
	}}
	wedge(f)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = f.Next(ctx)
	require.ErrorIs(t, err, ErrFeedClosed)
	assert.Equal(t, 1, opens, "the recovery reopened once, with Close landing inside it")
	assert.Empty(t, consumerNames(t, s))
}

// TestFeedReopenRacingCloseIsReleased drives install directly: a consumer
// opened after Close is deleted and the feed stays closed.
func TestFeedReopenRacingCloseIsReleased(t *testing.T) {
	s, _ := feedStore(t)
	f := openWedgeable(t, s)
	consumer, it, err := f.open(context.Background())
	require.NoError(t, err)
	require.Len(t, consumerNames(t, s), 2)
	require.NoError(t, f.Close())
	assert.False(t, f.install(consumer, it))
	assert.Empty(t, consumerNames(t, s))
	_, err = f.Next(context.Background())
	require.ErrorIs(t, err, ErrFeedClosed)
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
		require.ErrorIs(t, last, ErrFeedClosed, "round %d", round)
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
	require.NoError(t, release(stall.Stream, consumer, it))
	require.NoError(t, f.Close())
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrFeedClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Next still blocked in the idle check after Close")
	}
}
