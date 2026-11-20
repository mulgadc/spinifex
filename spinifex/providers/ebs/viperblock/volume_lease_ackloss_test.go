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
