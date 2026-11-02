package jetstream_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNATSOrderedFeedStallRepro reproduces, with nats.go alone and no store
// code, an ordered Messages iterator idling with stored messages pending
// after a full cluster restart. Opt-in: it is slow and probabilistic.
func TestNATSOrderedFeedStallRepro(t *testing.T) {
	rounds, _ := strconv.Atoi(os.Getenv("NATS_ORDERED_REPRO_ROUNDS"))
	if rounds < 1 {
		t.Skip("set NATS_ORDERED_REPRO_ROUNDS to run")
	}
	var opts []jsapi.PullMessagesOpt
	if v := os.Getenv("NATS_ORDERED_REPRO_EXPIRY"); v != "" {
		d, err := time.ParseDuration(v)
		require.NoError(t, err)
		opts = append(opts, jsapi.PullExpiry(d))
	}
	clustersize.RedeclareForTest(t, 3)
	c := startCluster(t)
	ctx := context.Background()
	// NATS_ORDERED_REPRO_TOPOLOGY=store reads SQS_M_0 among the store's
	// streams and buckets, with the store's consumer config, but no store code.
	streamName, subject := "REPRO", "repro.q%d"
	cfg := jsapi.OrderedConsumerConfig{HeadersOnly: true}
	if os.Getenv("NATS_ORDERED_REPRO_TOPOLOGY") == "store" {
		c.store(t)
		streamName, subject = sqsjs.StreamName(0), "sqs.m.0.q%d"
		cfg.InactiveThreshold = time.Minute
	} else {
		_, err := c.js.CreateStream(ctx, jsapi.StreamConfig{Name: "REPRO", Subjects: []string{"repro.>"}, Replicas: 3})
		require.NoError(t, err)
	}

	total := 0
	stalls := 0
	for round := range rounds {
		for i := range 1500 {
			_, err := c.js.Publish(ctx, fmt.Sprintf(subject, i%25), []byte("x"))
			require.NoError(t, err)
		}
		total += 1500
		c.stop()
		c.start()

		stream, err := c.js.Stream(ctx, streamName)
		require.NoError(t, err)
		cons, err := stream.OrderedConsumer(ctx, cfg)
		require.NoError(t, err)
		it, err := cons.Messages(opts...)
		require.NoError(t, err)

		var got uint64
		var maxGap time.Duration
		start, prev := time.Now(), time.Now()
		snapped := false
		for got < uint64(total) {
			nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			msg, err := it.Next(jsapi.NextContext(nctx))
			cancel()
			if err != nil {
				if !snapped {
					snapped = true
					stalls++
					info, ierr := cons.Info(ctx)
					if ierr == nil {
						t.Logf("round %d stall after seq %d: consumer %s delivered_stream=%d pending=%d waiting=%d err=%v",
							round, got, info.Name, info.Delivered.Stream, info.NumPending, info.NumWaiting, err)
					} else {
						t.Logf("round %d stall after seq %d: info error %v, next error %v", round, got, ierr, err)
					}
				}
				if time.Since(start) > 90*time.Second {
					t.Fatalf("round %d: no progress for 90s at seq %d", round, got)
				}
				continue
			}
			md, err := msg.Metadata()
			require.NoError(t, err)
			require.Equal(t, got+1, md.Sequence.Stream, "round %d: ordered delivery", round)
			got = md.Sequence.Stream
			if gap := time.Since(prev); gap > maxGap {
				maxGap = gap
			}
			prev = time.Now()
		}
		it.Stop()
		t.Logf("round %d: %d msgs in %dms max_gap_ms=%d", round, got, time.Since(start).Milliseconds(), maxGap.Milliseconds())
	}
	t.Logf("stalled rounds: %d of %d", stalls, rounds)
}
