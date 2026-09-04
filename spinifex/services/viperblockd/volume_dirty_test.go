package viperblockd

//test:in-package — the dirty marker, its bucket and acquireVolumeLease are all
//unexported, and the takeover they implement has no exported surface.

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/services/viperblockd/vbwire"
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
	require.NoError(t, gone.mark(t.Context(), volumeName, 1, "seal volume: predastore unreachable"))

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
	require.NoError(t, gone.mark(t.Context(), volumeName, 1, "seal volume: predastore unreachable"))

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
	require.NoError(t, cfg.dirty.mark(t.Context(), volumeName, 1, "seal volume: boom"))

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
	require.NoError(t, owner.dirty.mark(t.Context(), volumeName, 7, "seal volume: boom"))

	owner.clearVolumeDirty(t.Context(), volumeName, 7)

	_, ok := owner.dirty.holder(t.Context(), volumeName)
	assert.False(t, ok, "a sealed volume must leave no marker behind")
}

// TestVolumeDirty_ClearIsIdempotent covers the ordinary path, where every
// successful unmount clears a marker that may already be gone.
func TestVolumeDirty_ClearIsIdempotent(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	dirty := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, dirty.clear(t.Context(), "vol-dirtynevermarked", 1),
		"clearing an unmarked volume is the normal case, not an error")
}

// TestVolumeDirty_ClearDeclinesAMarkerAnotherNodeOwns is the safety the
// generation buys. A node that comes back after a takeover still has local
// state to seal, and its seal says nothing about the copy that moved on —
// clearing there would erase the record that the winner's writes are unsealed.
func TestVolumeDirty_ClearDeclinesAMarkerAnotherNodeOwns(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyclearnotours"

	winner := newTestDirty(t, natsURL, "node-b")
	require.NoError(t, winner.mark(t.Context(), volumeName, 9, "took over from node-a"))

	returned := &Config{dirty: newTestDirty(t, natsURL, "node-a")}
	returned.clearVolumeDirty(t.Context(), volumeName, 4)

	record, ok := winner.holder(t.Context(), volumeName)
	require.True(t, ok, "a returning node must not clear the marker that moved past it")
	assert.Equal(t, "node-b", record.Owner)
	assert.EqualValues(t, 9, record.Generation)
}

// TestVolumeDirty_ClearDeclinesAnOlderGenerationOfTheSameNode covers the same
// node reopening a volume it already held. The stale export's clear must not
// remove the marker the newer one wrote.
func TestVolumeDirty_ClearDeclinesAnOlderGenerationOfTheSameNode(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtyclearoldgen"

	owner := &Config{dirty: newTestDirty(t, natsURL, "node-a")}
	require.NoError(t, owner.dirty.mark(t.Context(), volumeName, 12, "reopened"))

	owner.clearVolumeDirty(t.Context(), volumeName, 5)

	record, ok := owner.dirty.holder(t.Context(), volumeName)
	require.True(t, ok, "an older export clearing would leave the live one unrecorded")
	assert.EqualValues(t, 12, record.Generation)
}

// TestVolumeDirty_MarkRefusesToOverwriteALaterGeneration stops a returning node
// renaming itself as the current copy. Placement reads this marker, so an
// overwrite here would point a later takeover at the stale node.
func TestVolumeDirty_MarkRefusesToOverwriteALaterGeneration(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtymarkstale"

	winner := newTestDirty(t, natsURL, "node-b")
	require.NoError(t, winner.mark(t.Context(), volumeName, 9, "took over from node-a"))

	stale := newTestDirty(t, natsURL, "node-a")
	err := stale.mark(t.Context(), volumeName, 4, "seal volume: boom")
	require.ErrorIs(t, err, errDirtyMarkerSuperseded)

	record, ok := winner.holder(t.Context(), volumeName)
	require.True(t, ok)
	assert.Equal(t, "node-b", record.Owner, "the later generation still owns the marker")
}

// TestVolumeDirty_OpenFailsWhenTheMarkerCannotBeWritten pins the ordering the
// marker depends on. A volume opened without one is a volume whose writes
// nothing records, so a later takeover could not tell they existed — and the
// lease has to go back, or the volume is stranded on a node not writing it.
func TestVolumeDirty_OpenFailsWhenTheMarkerCannotBeWritten(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-dirtymarkunwritable"

	// A generation no lease in a fresh bucket can reach, so this node's own
	// mark is refused as stale.
	blocker := newTestDirty(t, natsURL, "node-a")
	require.NoError(t, blocker.mark(t.Context(), volumeName, 1<<40, "held by a later generation"))

	cfg := &Config{
		leases: newTestLeases(t, natsURL, "node-a"),
		dirty:  newTestDirty(t, natsURL, "node-a"),
	}

	_, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.Error(t, err, "a volume whose writes cannot be recorded must not be opened")

	owner, held := cfg.leases.currentOwner(t.Context(), volumeName)
	assert.False(t, held && owner == "node-a",
		"the lease has to be released, or the volume is locked to a node that is not writing it")
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
	require.NoError(t, dirty.mark(t.Context(), "vol-dirtylisted", 1, "seal volume: predastore unreachable"))

	unsealed, err := vbwire.ListUnsealedVolumes(t.Context(), nc)
	require.NoError(t, err)
	require.Len(t, unsealed, 1)
	assert.Equal(t, "vol-dirtylisted", unsealed[0].VolumeID)
	assert.Equal(t, "node-a", unsealed[0].Owner)
	assert.Contains(t, unsealed[0].Reason, "predastore unreachable")
}
