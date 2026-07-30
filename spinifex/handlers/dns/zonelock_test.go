package dns

import (
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shrinkZoneLockWaits keeps contention tests fast without changing the code
// path, restoring the production budgets afterwards.
func shrinkZoneLockWaits(t *testing.T, waitFor time.Duration) {
	t.Helper()
	origWait, origStep, origMax := zoneLockWaitFor, zoneLockStep, zoneLockStepMax
	zoneLockWaitFor, zoneLockStep, zoneLockStepMax = waitFor, time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		zoneLockWaitFor, zoneLockStep, zoneLockStepMax = origWait, origStep, origMax
	})
}

func TestZoneLockIsMutuallyExclusive(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, 50*time.Millisecond)

	a := newZoneLocker(nc, "node1")
	b := newZoneLocker(nc, "node2")

	held, err := a.lockZone(t.Context(), "spx3.net")
	require.NoError(t, err)
	require.NotNil(t, held)

	// A second holder must not get in while the first has it.
	_, err = b.lockZone(t.Context(), "spx3.net")
	require.Error(t, err)
	assert.ErrorContains(t, err, "contended")

	held.Release(t.Context())

	// Released, so the peer now succeeds.
	second, err := b.lockZone(t.Context(), "spx3.net")
	require.NoError(t, err)
	require.NotNil(t, second)
	second.Release(t.Context())
}

func TestZoneLockDistinctZonesDoNotBlock(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, 50*time.Millisecond)

	a := newZoneLocker(nc, "node1")
	b := newZoneLocker(nc, "node2")

	first, err := a.lockZone(t.Context(), "spx3.net")
	require.NoError(t, err)
	defer first.Release(t.Context())

	// A held zone must never stall a write to an unrelated zone.
	other, err := b.lockZone(t.Context(), "compute.internal")
	require.NoError(t, err)
	require.NotNil(t, other)
	other.Release(t.Context())
}

func TestZoneLockSerialisesConcurrentHolders(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, 5*time.Second)

	const holders = 8
	var (
		mu       sync.Mutex
		inside   int
		maxSeen  int
		acquired int
	)

	var wg sync.WaitGroup
	for i := range holders {
		wg.Go(func() {
			l := newZoneLocker(nc, "node"+string(rune('a'+i)))
			lock, err := l.lockZone(t.Context(), "spx3.net")
			if err != nil {
				return
			}
			defer lock.Release(t.Context())

			mu.Lock()
			inside++
			acquired++
			maxSeen = max(maxSeen, inside)
			mu.Unlock()

			// Hold long enough that an unserialised peer would overlap here.
			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
		})
	}
	wg.Wait()

	assert.Equal(t, 1, maxSeen, "at most one holder may be inside the lock at a time")
	assert.Equal(t, holders, acquired, "every holder should get the lock within the wait budget")
}

func TestZoneLockWithoutNATSIsNoop(t *testing.T) {
	// A writer with no connection has no queue-group peer, so there is nothing to
	// serialise against and locking must not fail the write.
	l := newZoneLocker(nil, "node1")
	lock, err := l.lockZone(t.Context(), "spx3.net")
	require.NoError(t, err)
	assert.Nil(t, lock)
	lock.Release(t.Context()) // nil receiver must be safe
}

func TestZoneLockBindConnUpgradesLocker(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	shrinkZoneLockWaits(t, 50*time.Millisecond)

	// Built without a conn (so a no-op), then given one at Subscribe time.
	l := newZoneLocker(nil, "node1")
	l.bindConn(nc)

	lock, err := l.lockZone(t.Context(), "spx3.net")
	require.NoError(t, err)
	require.NotNil(t, lock, "a locker that adopted a conn must really lock")
	defer lock.Release(t.Context())

	_, err = newZoneLocker(nc, "node2").lockZone(t.Context(), "spx3.net")
	assert.ErrorContains(t, err, "contended")
}

func TestZoneLockLeaseExpiryIsDetectable(t *testing.T) {
	held := &zoneLock{acquired: time.Now().Add(-2 * zoneLockTTL)}
	assert.True(t, held.Expired(), "a write must abort rather than land outside its lease")

	fresh := &zoneLock{acquired: time.Now()}
	assert.False(t, fresh.Expired())

	var absent *zoneLock
	assert.False(t, absent.Expired(), "a no-op lock never blocks a write")
}

func TestZoneLockKeyNormalises(t *testing.T) {
	// DNS names are already legal KV keys, so this only normalises case and
	// guards odd input.
	assert.Equal(t, "spx3.net", zoneLockKey("SPX3.Net."))
	assert.Equal(t, "compute.internal", zoneLockKey("  compute.internal  "))
	assert.Equal(t, "_apex", zoneLockKey(""))
	assert.Equal(t, "_apex", zoneLockKey("."))
	assert.Equal(t, "a_b.net", zoneLockKey("a b.net"))
	assert.Equal(t, "zone-1.net", zoneLockKey("zone-1.net"))

	// Distinct zones must never collide onto one key.
	assert.NotEqual(t, zoneLockKey("a.spx3.net"), zoneLockKey("b.spx3.net"))
}
