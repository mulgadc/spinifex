package kvsync_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunElection_FiresEachEdgeOncePerTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(2)
		l := kvsync.NewLease(s[0], leaseKey, "node-a", testTTL)

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

		synctest.Wait()
		requireReceived(t, gained, "the first attempt on a free key must gain")

		// Another holder takes the key, so the next renewal loses it.
		takeKey(t, s[1])
		advance(testTTL / 3)
		requireReceived(t, lostEdge, "a lost renewal must fire onLost")

		// The election retries every ttl/3: the first retry sees the taker's
		// key, and the fourth takes it once that key has stood for a TTL.
		advance(4*testTTL/3 - time.Nanosecond)
		requireNotReceived(t, gained, "the election took the key before a full TTL of the taker's revision")
		advance(time.Nanosecond)
		requireReceived(t, gained, "the election must regain the lease after a TTL")

		cancel()
		<-done
		requireReceived(t, lostEdge, "stopping the election must fire onLost for the held lease")
		assert.Equal(t, int32(2), gains.Load())
		assert.Equal(t, int32(2), losses.Load())
		assert.Zero(t, revision(t, s[0], leaseKey), "stopping the election must release the key")
	})
}

func TestRunElection_FailedOnGainedStandsDownWithoutOnLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := memStores(1)
		l := kvsync.NewLease(s[0], leaseKey, "node-a", testTTL)

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
		synctest.Wait()
		requireReceived(t, attempted, "the first attempt must reach onGained")
		assert.False(t, l.Held(), "a failed onGained must stand the lease down")
		assert.Zero(t, revision(t, s[0], leaseKey), "standing down must free the key for a healthy node")

		cancel()
		<-done
		assert.Zero(t, losses.Load(), "a lease never gained is never lost")
	})
}

func requireReceived(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	default:
		require.FailNow(t, msg)
	}
}

func requireNotReceived(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
		require.FailNow(t, msg)
	default:
	}
}
