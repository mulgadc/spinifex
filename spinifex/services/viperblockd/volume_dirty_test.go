package viperblockd

//test:in-package — the dirty marker, its bucket and acquireVolumeLease are all
//unexported, and the takeover they implement has no exported surface.

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

// TestVolumeDirty_MarkedOnOpenNotOnSealFailure is the property the marker is
// built for. A node killed mid-write never reaches its seal, so a marker written
// only when a seal fails is absent in exactly the case it exists to describe.
func TestVolumeDirty_MarkedOnOpenNotOnSealFailure(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtymarkedonopen"

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-a"),
		dirty:  newTestDirty(t, natsURL, "node-a"),
	}

	_, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)

	record, ok := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, ok, "opening a volume must record that its writes are not yet on the backend")
	assert.Equal(t, "node-a", record.Owner)
}

// TestVolumeDirty_DoesNotRefuseAnotherNode locks the availability half. Instance
// start forwards to the node that last ran the instance and only falls back once
// that node has failed its window, so refusing here would leave the instance
// unable to run anywhere rather than running with a slightly older copy.
func TestVolumeDirty_DoesNotRefuseAnotherNode(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtytakeoverallowed"

	gone := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, gone.mark(t.Context(), volumeName, "seal volume: predastore unreachable"))

	survivor := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}

	lease, err := survivor.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err, "a volume must not be pinned to a node that is gone")
	require.NotNil(t, lease)
}

// TestVolumeDirty_TakeoverNamesThePreviousHolder pins the record left behind. A
// takeover silently loses whatever node-a never sealed, so the marker has to
// carry who held it and why for the operator who reads it afterwards.
func TestVolumeDirty_TakeoverNamesThePreviousHolder(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtytakeoverrecorded"

	gone := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, gone.mark(t.Context(), volumeName, "seal volume: predastore unreachable"))

	survivor := &Config{
		leases: newTestLeases(t, natsURL, "node-b"),
		dirty:  newTestDirty(t, natsURL, "node-b"),
	}
	_, err := survivor.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)

	record, ok := survivor.dirty.holder(t.Context(), volumeName)
	require.True(t, ok)
	assert.Equal(t, "node-b", record.Owner, "the node now holding the writes owns the marker")
	assert.Contains(t, record.Reason, "node-a", "the takeover must name the node whose writes were left behind")
	assert.Contains(t, record.Reason, "predastore unreachable",
		"why the previous holder could not seal is the operator's only lead")
}

// TestVolumeDirty_OwnerReopeningIsNotATakeover guards against a node warning
// about itself. Reopening a volume this node already holds is the ordinary
// restart path, not a lost-writes event.
func TestVolumeDirty_OwnerReopeningIsNotATakeover(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyownerreopens"

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-a"),
		dirty:  newTestDirty(t, natsURL, "node-a"),
	}
	require.NoError(t, cfg.dirty.mark(t.Context(), volumeName, "seal volume: boom"))

	_, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)

	record, ok := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, ok)
	assert.NotContains(t, record.Reason, "took over",
		"a node reopening its own volume has taken over from nobody")
}

// TestVolumeDirty_ClearedAfterASuccessfulSeal pins that the marker is not a
// one-way door: once the writes reach the backend the volume is clean again and
// carries no placement preference.
func TestVolumeDirty_ClearedAfterASuccessfulSeal(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyclearsonseal"

	owner := &Config{dirty: newTestDirty(t, natsURL, "node-a")}
	require.NoError(t, owner.dirty.mark(t.Context(), volumeName, "seal volume: boom"))

	owner.clearVolumeDirty(t.Context(), volumeName)

	_, ok := owner.dirty.holder(t.Context(), volumeName)
	assert.False(t, ok, "a sealed volume must leave no marker behind")
}

// TestVolumeDirty_ClearIsIdempotent covers the ordinary path, where every
// successful unmount clears a marker that may already be gone.
func TestVolumeDirty_ClearIsIdempotent(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	dirty := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, dirty.clear(t.Context(), "vol-dirtynevermarked"),
		"clearing an unmarked volume is the normal case, not an error")
}

// TestVolumeDirty_NoStoreDoesNotBlockMounts pins the degraded case. A daemon
// with no marker store still has the lease for live exclusion, and refusing
// every mount because the bucket is missing is a worse failure than the one
// being described.
func TestVolumeDirty_NoStoreDoesNotBlockMounts(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{leases: newTestLeases(t, natsURL, "node-b")}

	lease, err := cfg.acquireVolumeLease(t.Context(), "vol-dirtynostore")
	require.NoError(t, err)
	require.NotNil(t, lease)
}

// TestVolumeDirty_ListReportsTheHolder covers the operator surface behind
// 'spx admin volumes unsealed', which is the only way to see a volume whose
// writes are stranded on a node that is not coming back.
func TestVolumeDirty_ListReportsTheHolder(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	dirty := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, dirty.mark(t.Context(), "vol-dirtylisted", "seal volume: predastore unreachable"))

	unsealed, err := ListUnsealedVolumes(t.Context(), nc)
	require.NoError(t, err)
	require.Len(t, unsealed, 1)
	assert.Equal(t, "vol-dirtylisted", unsealed[0].VolumeID)
	assert.Equal(t, "node-a", unsealed[0].Owner)
	assert.Contains(t, unsealed[0].Reason, "predastore unreachable")
}
