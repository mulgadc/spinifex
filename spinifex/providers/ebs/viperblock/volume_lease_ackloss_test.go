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
	published := start.Add(3 * time.Second)
	leases.kv = &slowCreateKV{ackLossKV: ackLossKV{KeyValue: leases.kv}, clock: clock, after: published}

	lease, err := leases.acquire(t.Context(), "vol-acklosspublish")
	require.NoError(t, err)
	lease.stop()
	<-lease.done
	created := lease.revision

	clock.set(start.Add(10 * time.Second))
	require.True(t, lease.renew(t.Context()), "the refused renewal re-reads the stored publish")

	lease.mu.Lock()
	revision := lease.revision
	lease.mu.Unlock()
	require.Greater(t, revision, created, "the stored publish revision must be adopted")
	require.Equal(t, published, lease.lastConfirmed(),
		"the publish is the unacknowledged write, so the lease is confirmed from its send and no later")
	clock.set(published.Add(volumeLeaseValidity))
	require.True(t, lease.expiredLocally(), "validity ends exactly 40 s after the publish was sent")
}

// slowCreateKV moves the clock forward once the Create returns, so the
// generation publish that follows is sent at a later time than the Create.
type slowCreateKV struct {
	ackLossKV

	clock *fakeClock
	after time.Time
}

func (k *slowCreateKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	k.clock.set(k.after)
	return rev, err
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
	lease.mu.Lock()
	defer lease.mu.Unlock()
	require.Equal(t, uint64(200), lease.revision, "a stale read must not roll the revision back")
}

// scriptedReadKV refuses every update and answers reads from a script, which
// is how a stale replica looks to the lease.
type scriptedReadKV struct {
	jetstream.KeyValue

	value     []byte
	revisions []uint64
}

func (k *scriptedReadKV) Update(context.Context, string, []byte, uint64) (uint64, error) {
	return 0, jetstream.ErrKeyRevisionMismatch
}

func (k *scriptedReadKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	rev := k.revisions[0]
	k.revisions = k.revisions[1:]
	return stubEntry{value: k.value, revision: rev}, nil
}

// Stale reads that return an older revision and then the acknowledged one must
// neither roll the lease back nor pass the acknowledged revision off as newer,
// or a later read of the real write confirms from too late a send.
func TestVolumeLease_StaleReadsCannotStretchValidity(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	start := time.Now()
	clock := &fakeClock{now: start}
	leases.now = clock.Now

	lease, err := leases.acquire(t.Context(), "vol-acklossstale")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	// R2 = 20 was acknowledged at 0 s; R3 = 30 was sent at 20 s and stored, its
	// acknowledgement lost.
	lease.mu.Lock()
	lease.revision = 20
	lease.confirmed = start
	lease.unackedSince = start.Add(20 * time.Second)
	lease.mu.Unlock()
	leases.kv = &scriptedReadKV{KeyValue: leases.kv, value: leaseEntry(t, "node-a", lease.generation), revisions: []uint64{10, 20, 30}}

	clock.set(start.Add(25 * time.Second))
	require.True(t, lease.renew(t.Context()), "a stale read of R1 keeps the lease")
	require.True(t, lease.renew(t.Context()), "a read of the acknowledged R2 keeps the lease")
	require.Equal(t, start, lease.lastConfirmed(), "neither stale read proves a newer write was stored")

	require.True(t, lease.renew(t.Context()), "a read of R3 proves the write sent at 20 s was stored")
	require.Equal(t, start.Add(20*time.Second), lease.lastConfirmed(),
		"validity must run from 20 s, the send of the write R3 came from, and no later")
}

// A lease already given up is not changed by a later re-read of its own write,
// and is not fenced a second time.
func TestVolumeLease_AReReadAfterTheLeaseIsLostChangesNothing(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	leases := newTestLeases(t, natsURL, "node-a")
	fences := make(chan leaseLossKind, 2)
	leases.onLost = func(_ context.Context, _ string, kind leaseLossKind) { fences <- kind }

	lease, err := leases.acquire(t.Context(), "vol-acklosslost")
	require.NoError(t, err)
	lease.stop()
	<-lease.done

	lease.mu.Lock()
	lease.lost = true
	lease.unackedSince = lease.confirmed.Add(time.Second)
	before, revision := lease.confirmed, lease.revision
	lease.mu.Unlock()
	leases.kv = &scriptedReadKV{KeyValue: leases.kv, value: leaseEntry(t, "node-a", lease.generation), revisions: []uint64{revision + 5}}

	require.False(t, lease.renew(t.Context()), "a lost lease stays lost")
	require.Equal(t, before, lease.lastConfirmed())
	select {
	case kind := <-fences:
		t.Fatalf("a lease already given up was fenced again (%v)", kind)
	case <-time.After(200 * time.Millisecond):
	}
}
