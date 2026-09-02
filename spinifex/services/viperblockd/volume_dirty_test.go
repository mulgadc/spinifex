package viperblockd

//test:in-package — the dirty marker, its bucket and acquireVolumeLease are all
//unexported, and the exclusion they implement has no exported surface.

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDirty binds a dirty-marker store for owner against natsURL.
func newTestDirty(t *testing.T, natsURL, owner string) *volumeDirty {
	t.Helper()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	dirty, err := newVolumeDirty(t.Context(), nc, owner)
	require.NoError(t, err)
	return dirty
}

// TestVolumeDirty_SurvivesTheLeaseHolderGoingAway is the property the lease
// cannot provide. A node that dies holding un-uploaded writes stops renewing,
// its lease ages out, and without a durable marker nothing then refuses a start
// elsewhere against a stale backend checkpoint.
//
// The lease store for node-a is never created here, which is precisely the
// state of a node that is gone: no holder, no renewal, nothing to expire.
func TestVolumeDirty_SurvivesTheLeaseHolderGoingAway(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyoutlivesalease"

	gone := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, gone.mark(t.Context(), volumeName, "seal volume: predastore unreachable"))

	survivor := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}

	_, err := survivor.acquireVolumeLease(t.Context(), volumeName)
	require.ErrorIs(t, err, errVolumeDirtyElsewhere,
		"node-a holds the only current copy, so node-b must be refused however long node-a has been gone")
	assert.Contains(t, err.Error(), "node-a", "the refusal has to name the node holding the data")
	assert.Contains(t, err.Error(), "predastore unreachable",
		"the seal error is why the volume is pinned, so an operator should not have to correlate journals for it")
}

// TestVolumeDirty_OwnerCanStillOpenItsOwnVolume covers the other half: the node
// holding the data is the one that must be able to retry the seal, so the
// marker must never lock out its own author.
func TestVolumeDirty_OwnerCanStillOpenItsOwnVolume(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyownerreopens"

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-a"),
		dirty:  newTestDirty(t, natsURL, "node-a"),
	}
	require.NoError(t, cfg.dirty.mark(t.Context(), volumeName, "seal volume: boom"))

	lease, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err, "the node holding the data must be able to retry its own seal")
	require.NotNil(t, lease)
}

// TestVolumeDirty_ClearedAfterASuccessfulSeal pins that the refusal is not a
// one-way door. Once the writes reach the backend, any node may open the
// volume again — otherwise a single transient outage would strand it forever.
func TestVolumeDirty_ClearedAfterASuccessfulSeal(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyclearsonseal"

	owner := &Config{dirty: newTestDirty(t, natsURL, "node-a")}
	require.NoError(t, owner.dirty.mark(t.Context(), volumeName, "seal volume: boom"))

	other := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}
	_, err := other.acquireVolumeLease(t.Context(), volumeName)
	require.ErrorIs(t, err, errVolumeDirtyElsewhere)

	owner.clearVolumeDirty(t.Context(), volumeName)

	lease, err := other.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err, "a sealed volume must be openable anywhere again")
	require.NotNil(t, lease)
}

// TestVolumeDirty_ClearIsIdempotent covers the ordinary path, where every
// successful unmount clears a marker that was never written.
func TestVolumeDirty_ClearIsIdempotent(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	dirty := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, dirty.clear(t.Context(), "vol-dirtynevermarked"),
		"clearing an unmarked volume is the normal case, not an error")
}

// TestVolumeDirty_UnmarkedVolumeIsUnaffected guards against the check refusing
// the routine mount, which would take the whole cluster down rather than one
// volume.
func TestVolumeDirty_UnmarkedVolumeIsUnaffected(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}

	lease, err := cfg.acquireVolumeLease(t.Context(), "vol-dirtycleanvolume")
	require.NoError(t, err)
	require.NotNil(t, lease)
}

// TestVolumeDirty_NoStoreDoesNotBlockMounts pins the degraded case. A daemon
// with no marker store still has the lease for live exclusion, and refusing
// every mount because the bucket is missing is a worse failure than the one
// being guarded against.
func TestVolumeDirty_NoStoreDoesNotBlockMounts(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{leases: newTestLeases(t, natsURL, "node-b")}
	require.NoError(t, cfg.checkVolumeDirty(t.Context(), "vol-dirtynostore"))
}
