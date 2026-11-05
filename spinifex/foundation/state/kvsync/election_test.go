package kvsync_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunElection_FiresEachEdgeOncePerTransition(t *testing.T) {
	s := stores(t, 2)
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, c)

	gained := make(chan struct{}, 4)
	lostEdge := make(chan struct{}, 4)
	var gains, losses atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		kvsync.RunElection(ctx, l,
			func(context.Context) error { gains.Add(1); gained <- struct{}{}; return nil },
			func() { losses.Add(1); lostEdge <- struct{}{} })
	}()

	requireClosedRecv(t, gained, "the first attempt on a free key must gain")

	// Another holder takes the key, so the next renewal loses it.
	takeKey(t, s[1])
	waitForWaiters(t, c, 1)
	c.Advance(testTTL / 3)
	requireClosedRecv(t, lostEdge, "a lost renewal must fire onLost")

	// The election keeps retrying every ttl/3 and takes the taker's key once
	// it has seen it unchanged for a TTL.
	for range 4 {
		waitForWaiters(t, c, 1)
		c.Advance(testTTL / 3)
	}
	requireClosedRecv(t, gained, "the election must regain the lease after a TTL")

	cancel()
	<-done
	requireClosedRecv(t, lostEdge, "stopping the election must fire onLost for the held lease")
	assert.Equal(t, int32(2), gains.Load())
	assert.Equal(t, int32(2), losses.Load())
	assert.Zero(t, revision(t, s[0], leaseKey), "stopping the election must release the key")
}

func TestRunElection_FailedOnGainedStandsDownWithoutOnLost(t *testing.T) {
	s := stores(t, 1)
	c := kvsync.NewFakeClock()
	l := kvsync.NewLeaseWithClock(s[0], leaseKey, "node-a", testTTL, c)

	attempted := make(chan struct{}, 1)
	var losses atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		kvsync.RunElection(ctx, l,
			func(context.Context) error { attempted <- struct{}{}; return errors.New("cannot lead") },
			func() { losses.Add(1) })
	}()
	requireClosedRecv(t, attempted, "the first attempt must reach onGained")
	// The stopped renewal leaves its one timer behind, and the retry timer is
	// registered only after the stand-down has unlocked.
	waitForWaiters(t, c, 2)
	assert.False(t, l.Held(), "a failed onGained must stand the lease down")
	assert.Zero(t, revision(t, s[0], leaseKey), "standing down must free the key for a healthy node")

	cancel()
	<-done
	assert.Zero(t, losses.Load(), "a lease never gained is never lost")
}

func requireClosedRecv(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-waitCtx(t).Done():
		require.FailNow(t, msg)
	}
}
