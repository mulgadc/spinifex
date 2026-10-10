package viperblock

//test:in-package — abandonVolume, the lease store and MountedVolumes are all
//unexported, and the reason this differs from an unmount has no exported form.

import (
	"context"
	"testing"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The returning node's half of the fence. Its lease is not lost — nothing is
// wrong with this node — so the export would be renewed indefinitely and the
// instance would be stopped here and unable to start anywhere.
func TestVolumeAbandon_GivesUpTheExportAndTheLease(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-abandoned"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	pid := cfg.MountedVolumes[0].PID

	abandoned, err := cfg.abandonVolume(t.Context(), volumeName, "the instance moved")
	require.NoError(t, err)
	assert.True(t, abandoned)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	assert.Zero(t, mounted, "an export left up is a second writer, whoever decided to stop")
	assert.False(t, hostprocess.ProcessAlive(pid), "the export process has to be gone, not merely forgotten")
}

// The difference from a fence, and the reason this path is worth having. A
// fenced node has lost its lease and may not delete the key, so the winner waits
// out the TTL. Here the node still holds it and hands it back, so the winner can
// open the volume on its next attempt.
func TestVolumeAbandon_ReleasesTheKeySoTheNewOwnerNeedNotWaitOutTheTTL(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-abandonreleases"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	_, err := cfg.abandonVolume(t.Context(), volumeName, "the instance moved")
	require.NoError(t, err)

	owner, held := cfg.leases.currentOwner(t.Context(), volumeName)
	assert.False(t, held,
		"the lease was this node's to give back, and leaving it to expire makes the new owner wait for nothing")
	assert.Empty(t, owner)
}

// The one thing it must never do, for the fence's reason: this node's block map
// is behind the node that took the volume, and sealing publishes it over theirs.
func TestVolumeAbandon_DoesNotSeal(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-abandonnoseal"

	sealed := false
	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }

	_, err := cfg.abandonVolume(t.Context(), volumeName, "the instance moved")
	require.NoError(t, err)

	assert.False(t, sealed, "abandoning must never seal: this node's copy is behind the new owner's")
}

// Reporting success on a kill that did not take would tell the caller a writer
// stopped that is still running, which is worse than refusing.
func TestVolumeAbandon_KillFailureIsReportedAndLeavesTheVolumeMounted(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-abandonkillfails"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	cfg.MountedVolumes[0].PID = 0 // rejected by ForceKillProcess, so the kill cannot succeed

	abandoned, err := cfg.abandonVolume(t.Context(), volumeName, "the instance moved")
	require.Error(t, err, "a writer that is still running must not be reported as stopped")
	assert.False(t, abandoned)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	require.Equal(t, 1, mounted, "the entry has to stay so a retry re-attempts the kill")

	owner, held := cfg.leases.currentOwner(t.Context(), volumeName)
	require.True(t, held, "the lease must not be handed back while this node is still exporting")
	assert.Equal(t, "node-a", owner)
}

// The ordinary case. The caller is reconciling a whole instance's volumes, and
// most of them were never exported here — so this is not a failure, and treating
// it as one would make every recovery look broken.
func TestVolumeAbandon_NotExportedHereIsNotAnError(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{
		NodeName: "node-a",
		leases:   newTestLeases(t, natsURL, "node-a"),
		dirty:    newTestDirty(t, natsURL, "node-a"),
	}

	abandoned, err := cfg.abandonVolume(t.Context(), "vol-neverhere", "the instance moved")
	require.NoError(t, err)
	assert.False(t, abandoned, "nothing was torn down, and the caller is entitled to know that")
}

// The marker names whose writes the backend may be missing. The new owner takes
// it over when it opens the volume, so clearing it here would erase the only
// note that writes were lost.
func TestVolumeAbandon_LeavesTheDirtyMarkerToTheNewOwner(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-abandonkeepsmarker"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	_, err := cfg.abandonVolume(t.Context(), volumeName, "the instance moved")
	require.NoError(t, err)

	record, ok := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, ok, "abandoning must leave the marker behind for the new owner to take over")
	assert.Equal(t, "node-a", record.Owner)
}
