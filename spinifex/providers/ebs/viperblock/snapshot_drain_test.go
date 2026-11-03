package viperblock

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountedSnapshotFixture is a node with volumeID mounted over a real engine,
// whose base dir is where testutil's drain socket stand-in listens.
func mountedSnapshotFixture(t *testing.T, volumeID string) (cfg *Config, dataDir string, vb *viperblock.VB) {
	t.Helper()
	dataDir = testutil.SocketTempDir(t)
	vb = createTestVBWithState(t, volumeID)
	t.Cleanup(func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	})
	cfg = &Config{
		NodeName:       "node-a",
		BaseDir:        filepath.Join(dataDir, "viperblock"),
		MountedVolumes: []MountedVolume{{Name: volumeID, VB: vb}},
	}
	require.NoError(t, os.MkdirAll(cfg.BaseDir, 0o750))
	return cfg, dataDir, vb
}

func requireNoSnapshot(t *testing.T, vb *viperblock.VB, snapshotID string) {
	t.Helper()
	exists, err := snapshotConfigExists(vb, snapshotID)
	require.NoError(t, err)
	assert.False(t, exists, "a snapshot that could not be shown current must not be written")
}

// The snapshot reads the checkpoint the plugin flushes, so it must not start
// until the plugin has acknowledged the drain.
func TestSnapshotVolumeEngine_MountedVolumeIsDrainedBeforeTheSnapshot(t *testing.T) {
	const volumeID, snapshotID = "vol-drainfirst", "snap-drainfirst00001"
	cfg, dataDir, vb := mountedSnapshotFixture(t, volumeID)
	accepted, release := testutil.StartBlockingDrainSocket(t, dataDir, volumeID, "OK\n")

	type result struct {
		snapshot *ebsprovider.Snapshot
		err      error
	}
	done := make(chan result, 1)
	go func() {
		snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)
		done <- result{snapshot, err}
	}()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("a mounted volume's snapshot never asked the plugin to drain")
	}
	requireNoSnapshot(t, vb, snapshotID)
	release()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, ebsprovider.SnapshotStateCompleted, got.snapshot.State)
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot did not complete after the drain acked")
	}
}

// A plugin that answers without confirming the flush leaves the checkpoint
// unproven, so the snapshot fails rather than capturing older data.
func TestSnapshotVolumeEngine_RefusedDrainFailsTheSnapshot(t *testing.T) {
	const volumeID, snapshotID = "vol-drainerr", "snap-drainerr0000001"
	cfg, dataDir, vb := mountedSnapshotFixture(t, volumeID)
	testutil.StartDrainSocket(t, dataDir, volumeID, "ERR\n")

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeInternal, snapshotErrorCode(err))
	requireNoSnapshot(t, vb, snapshotID)
}

// A socket file nobody listens on is a plugin that died mid-connection, whose
// writes were never drained.
func TestSnapshotVolumeEngine_StaleDrainSocketFailsTheSnapshot(t *testing.T) {
	const volumeID, snapshotID = "vol-drainstale", "snap-drainstale00001"
	cfg, _, vb := mountedSnapshotFixture(t, volumeID)
	sockDir := filepath.Join(cfg.BaseDir, volumeID)
	require.NoError(t, os.MkdirAll(sockDir, 0o750))
	ln, err := net.Listen("unix", filepath.Join(sockDir, "snapshot.sock"))
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	requireNoSnapshot(t, vb, snapshotID)
}

// The plugin serves the socket only while a client is connected, and drains
// on disconnect. An export with no client has nothing buffered to flush.
func TestSnapshotVolumeEngine_ExportWithoutAClientNeedsNoDrain(t *testing.T) {
	const volumeID, snapshotID = "vol-drainidle", "snap-drainidle000001"
	cfg, _, _ := mountedSnapshotFixture(t, volumeID)

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snapshot.State)
}

// A client between connections holds the volume lock without serving the
// socket: its writes are neither drainable nor sealed yet, which clears soon.
func TestSnapshotVolumeEngine_ExportBetweenConnectionsIsRetryLater(t *testing.T) {
	const volumeID, snapshotID = "vol-drainsettle", "snap-drainsettle0001"
	cfg, _, vb := mountedSnapshotFixture(t, volumeID)
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = viperblock.ReleaseVolumeLock(lock) })

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportSettling)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, snapshotErrorCode(err))
	requireNoSnapshot(t, vb, snapshotID)
}
