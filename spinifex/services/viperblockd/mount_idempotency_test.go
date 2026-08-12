package viperblockd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMountVolume_AlreadyMountedReturnsExistingExport pins the guard at
// mountVolume rather than at one handler. The legacy ebs.<node>.mount subject
// is the route production takes for instance launch and hot-attach, so a guard
// that only sits in handlePublishVolume leaves a retried attach free to start a
// second nbdkit against a volume this node already exports.
func TestMountVolume_AlreadyMountedReturnsExistingExport(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)

	const volumeName = "vol-mountidempotent01"
	const existingURI = "nbd://127.0.0.1:10809"
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name:     volumeName,
		NBDURI:   existingURI,
		PID:      4242,
		ReadOnly: false,
	})

	resp, err := mountVolume(context.Background(), cfg, nil, volumeName, false)

	require.NoError(t, err)
	assert.True(t, resp.Mounted)
	assert.Equal(t, existingURI, resp.URI, "the running export must be handed back, not replaced")
	assert.Len(t, cfg.MountedVolumes, 1, "a second entry means a second nbdkit was started against one volume")
}

// TestMountVolume_AlreadyMountedOtherModeRefused covers the half the idempotent
// return cannot answer: nbdkit's access mode is fixed when it starts, so a
// remount asking for the other mode has to be refused rather than served with
// the running export.
func TestMountVolume_AlreadyMountedOtherModeRefused(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)

	const volumeName = "vol-mountidempotent02"
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name:     volumeName,
		NBDURI:   "nbd://127.0.0.1:10809",
		ReadOnly: true,
	})

	resp, err := mountVolume(context.Background(), cfg, nil, volumeName, false)

	require.Error(t, err)
	assert.False(t, resp.Mounted)
	assert.Contains(t, err.Error(), "already mounted read_only=true")
	assert.Len(t, cfg.MountedVolumes, 1)
}
