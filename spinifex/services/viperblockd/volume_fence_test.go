package viperblockd

//test:in-package — the fence, the lease store and MountedVolumes are all
//unexported, and the decision the fence makes has no exported surface.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fencedConfig builds a Config owning volumeName with one mounted entry, as a
// node that has an export up would have. PID 0 so the fence's KillProcess is a
// no-op rather than signalling something real.
func fencedConfig(t *testing.T, natsURL, owner, volumeName string) *Config {
	t.Helper()

	cfg := &Config{
		NodeName: owner,
		leases:   newTestLeases(t, natsURL, owner),
		dirty:    newTestDirty(t, natsURL, owner),
	}
	cfg.leases = cfg.bindLeaseFence(cfg.leases)

	lease, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)
	cfg.MountedVolumes = []MountedVolume{{Name: volumeName, Lease: lease}}
	return cfg
}

// TestVolumeFence_TearsDownWhenAnotherNodeHoldsTheLease is the property the
// fence exists for. A node that has lost its lease is a second writer, and the
// only way to stop being one without a backend that can refuse is to give up
// the export.
func TestVolumeFence_TearsDownWhenAnotherNodeHoldsTheLease(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencelost"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	// node-b takes the volume, which is what the renewal would discover.
	winner := newTestLeases(t, natsURL, "node-b")
	require.NoError(t, winner.kv.Delete(t.Context(), volumeName))
	_, err := winner.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	cfg.onVolumeLeaseLost(t.Context(), volumeName)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	assert.Zero(t, mounted, "a fenced volume must not be left exported: the export is the second writer")
}

// TestVolumeFence_ReclaimsALapsedEntryInsteadOfStoppingTheGuest is the half
// that keeps the fence from being worse than the problem. An entry that aged
// out under JetStream pressure with nobody claiming it is not a takeover, and
// killing a healthy guest for one would be a failure this code invented.
func TestVolumeFence_ReclaimsALapsedEntryInsteadOfStoppingTheGuest(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencelapsed"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	// The entry ages out with no successor, which is what a JetStream stall
	// looks like from here.
	require.NoError(t, cfg.leases.kv.Delete(t.Context(), volumeName))
	cfg.leases.mu.Lock()
	delete(cfg.leases.held, volumeName)
	cfg.leases.mu.Unlock()

	cfg.onVolumeLeaseLost(t.Context(), volumeName)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	released := cfg.MountedVolumes
	cfg.mu.Unlock()
	require.Equal(t, 1, mounted, "a lapsed lease nobody took must not cost the guest its disk")
	assert.NotNil(t, released[0].Lease, "the reclaimed lease has to be the one the entry now renews")

	owner, held := cfg.leases.currentOwner(t.Context(), volumeName)
	assert.True(t, held)
	assert.Equal(t, "node-a", owner, "reclaiming means this node holds the entry again")
}

// TestVolumeFence_DoesNotSealOnTheWayOut pins the one thing the fence must
// never do. This node's copy is the stale one, so sealing it would publish an
// older state over the winner's — the corruption the fence exists to prevent.
func TestVolumeFence_DoesNotSealOnTheWayOut(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencenoseal"

	sealed := false
	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }

	cfg.fenceVolume(t.Context(), volumeName, "node-b")

	assert.False(t, sealed, "fencing must never seal: this node's copy is behind the winner's")
}

// TestVolumeFence_LeavesTheDirtyMarkerToTheWinner covers the record an operator
// reads afterwards. The winner takes the marker over when it opens the volume,
// so a fencing node clearing it would erase the only note that writes were lost.
func TestVolumeFence_LeavesTheDirtyMarkerToTheWinner(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencekeepsmarker"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	cfg.fenceVolume(t.Context(), volumeName, "node-b")

	record, ok := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, ok, "fencing must leave the marker behind for the winner to take over")
	assert.Equal(t, "node-a", record.Owner)
}

// TestVolumeFence_AlreadyUnmountedIsNotAnError covers the ordinary race, where
// a volume is unmounted normally between losing its lease and the fence running.
func TestVolumeFence_AlreadyUnmountedIsNotAnError(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{
		NodeName: "node-a",
		leases:   newTestLeases(t, natsURL, "node-a"),
		dirty:    newTestDirty(t, natsURL, "node-a"),
	}

	cfg.fenceVolume(t.Context(), "vol-fencenotmounted", "node-b")
}

// TestVolumeFencedSubject_IsNodeAddressed pins the routing. A fenced guest is on
// the node that lost the volume, so a subject without the node in it would reach
// daemons with nothing to stop and, on a queue group, miss the one that has.
func TestVolumeFencedSubject_IsNodeAddressed(t *testing.T) {
	assert.Equal(t, "ebs.bottlebrush.fenced", VolumeFencedSubject("bottlebrush"))
	assert.Equal(t, "ebs.fenced", VolumeFencedSubject(""),
		"a single-node daemon has no node name, and still has to hear its own fences")
}
