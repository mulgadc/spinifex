package viperblock

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ackLossKV stores every conditional update that would succeed and then reports
// the acknowledgement lost, while a rejected update and a read answer at once.
// That is the store the prod journals show during a lease incident.
type ackLossKV struct {
	jetstream.KeyValue
}

func (k *ackLossKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if _, err := k.KeyValue.Update(ctx, key, value, revision); err != nil {
		return 0, err
	}
	return 0, context.DeadlineExceeded
}

// fakeClock is the time a lease measures validity on, moved by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// TestVolumeLease_LostAcknowledgementsDoNotLapseALiveLease reproduces the
// 2026-11-20 fences. Each renewal is stored but its acknowledgement is lost, so
// the holder only ever learns of its writes by re-reading them; no peer exists.
func TestVolumeLease_LostAcknowledgementsDoNotLapseALiveLease(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	start := time.Now()
	clock := &fakeClock{now: start}
	leases.now = clock.Now

	lease, err := leases.acquire(t.Context(), "vol-ackloss")
	require.NoError(t, err)
	lease.stop()
	<-lease.done
	leases.kv = &ackLossKV{KeyValue: leases.kv}

	at := func(seconds int) { clock.set(start.Add(time.Duration(seconds) * time.Second)) }

	// The schedule the renew loop runs: an attempt at 10 s that times out at
	// 22 s, a retry at 25 s that is refused and re-reads, then another at 30 s.
	at(10)
	require.True(t, lease.renew(t.Context()))
	at(25)
	require.True(t, lease.renew(t.Context()), "the refused retry re-reads this node's own entry")
	at(30)
	require.True(t, lease.renew(t.Context()))

	at(41)
	require.False(t, lease.expiredLocally(),
		"the lease lapsed at 40 s although every renewal since 10 s was stored and no peer exists; "+
			"the store's own copy restarted its TTL no earlier than 10 s, so validity runs to 50 s")

	at(45)
	require.True(t, lease.renew(t.Context()), "the refused retry re-reads this node's own entry")
	at(60)
	require.False(t, lease.expiredLocally(),
		"the second re-read proves a write sent at 30 s was stored, so validity runs to 70 s")
}

// A generation publish stored without an acknowledgement is one of this lease's
// own writes too, and was sent at acquisition. A renewal that re-reads it must
// not confirm the lease from the renewal's later send, or validity outlives the
// server's TTL by the gap between the two.
func TestVolumeLease_AnUnacknowledgedGenerationPublishBoundsTheConfirmation(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	start := time.Now()
	clock := &fakeClock{now: start}
	leases.now = clock.Now
	leases.kv = &ackLossKV{KeyValue: leases.kv}

	lease, err := leases.acquire(t.Context(), "vol-acklosspublish")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	clock.set(start.Add(10 * time.Second))
	require.True(t, lease.renew(t.Context()), "the refused renewal re-reads the stored publish")

	require.False(t, lease.lastConfirmed().After(start),
		"the publish was sent at acquisition, so the lease cannot be confirmed from any later time")
	clock.set(start.Add(volumeLeaseValidity + time.Second))
	require.True(t, lease.expiredLocally(),
		"validity must end 40 s after the publish was sent, inside the server's 45 s TTL")
}

// A lease that has already lapsed on this node's clock is given up when a
// re-read lands, rather than revived before the watchdog notices the lapse.
func TestVolumeLease_AReReadAfterALapseDoesNotReviveTheLease(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	lost := make(chan leaseLossKind, 1)
	leases.onLost = func(_ context.Context, _ string, kind leaseLossKind) { lost <- kind }
	start := time.Now()
	clock := &fakeClock{now: start}
	leases.now = clock.Now

	lease, err := leases.acquire(t.Context(), "vol-acklosslapsed")
	require.NoError(t, err)
	lease.stop()
	<-lease.done
	leases.kv = &ackLossKV{KeyValue: leases.kv}

	clock.set(start.Add(10 * time.Second))
	require.True(t, lease.renew(t.Context()))
	clock.set(start.Add(volumeLeaseValidity + time.Second))
	require.False(t, lease.renew(t.Context()), "a re-read after the lapse must give the volume up")

	select {
	case kind := <-lost:
		require.Equal(t, leaseLostStalled, kind)
	case <-time.After(5 * time.Second):
		t.Fatal("the lapsed lease was not surrendered")
	}
}

// Only a revision newer than the last acknowledged one proves a later write
// was stored. A re-read returning an older revision confirms nothing.
func TestVolumeLease_AReReadOfAnOlderRevisionConfirmsNothing(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")

	lease, err := leases.acquire(t.Context(), "vol-acklossolder")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	before := lease.lastConfirmed()
	lease.mu.Lock()
	lease.revision = 200
	lease.unackedSince = before.Add(time.Second)
	lease.mu.Unlock()

	leases.kv = &conflictKV{
		KeyValue: leases.kv,
		code:     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		getValue: leaseEntry(t, "node-a", lease.generation),
	}
	require.True(t, lease.renew(t.Context()))
	require.Equal(t, before, lease.lastConfirmed(), "revision 99 is older than 200, so nothing new was stored")
}
