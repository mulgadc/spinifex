package viperblock

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	cfg, vb = mountedAt(t, volumeID, filepath.Join(dataDir, "viperblock"))
	return cfg, dataDir, vb
}

// mountedAt is a node with volumeID mounted over a real engine under baseDir.
func mountedAt(t *testing.T, volumeID, baseDir string) (*Config, *viperblock.VB) {
	t.Helper()
	vb := createTestVBWithState(t, volumeID)
	t.Cleanup(func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	})
	cfg := &Config{
		NodeName:       "node-a",
		BaseDir:        baseDir,
		MountedVolumes: []MountedVolume{{Name: volumeID, VB: vb}},
	}
	require.NoError(t, os.MkdirAll(cfg.BaseDir, 0o750))
	return cfg, vb
}

// leaveUnsealedWAL stands in for a client whose disconnect seal failed: the
// plugin keeps the volume's local WAL and writes no receipt.
func leaveUnsealedWAL(t *testing.T, cfg *Config, volumeID string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(cfg.BaseDir, volumeID, "wal", "chunks"), 0o750))
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

// The control plane issues no drain of its own, so this one is the only flush
// an attached volume's snapshot gets, and it must happen exactly once.
func TestSnapshotVolumeEngine_MountedVolumeIsDrainedOnce(t *testing.T) {
	const volumeID, snapshotID = "vol-drainonce", "snap-drainonce000001"
	cfg, dataDir, _ := mountedSnapshotFixture(t, volumeID)
	drains := testutil.StartDrainSocket(t, dataDir, volumeID, "OK\n")

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snapshot.State)
	assert.Equal(t, 1, drains())
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
	logs := captureLogs(t)

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportSettling)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, snapshotErrorCode(err))
	requireNoSnapshot(t, vb, snapshotID)
	// A plugin that never serves its socket refuses every retry, so each
	// refusal is logged with the socket an operator would look for.
	sockPath := filepath.Join(cfg.BaseDir, volumeID, "snapshot.sock")
	assert.True(t, slices.ContainsFunc(strings.Split(logs.String(), "\n"), func(line string) bool {
		return strings.Contains(line, "level=WARN") && strings.Contains(line, "volume="+volumeID) &&
			strings.Contains(line, "socket="+sockPath)
	}), "the refusal must be logged at WARN with the volume and socket: %s", logs.String())
}

// The plugin releases the lock even when its disconnect seal fails, so a free
// lock with WAL left behind and no receipt is writes that never reached the
// backend, and the checkpoint a snapshot would read predates them.
func TestSnapshotVolumeEngine_UnsealedLastClientFailsTheSnapshot(t *testing.T) {
	const volumeID, snapshotID = "vol-drainunsealed", "snap-drainunsealed01"
	cfg, _, vb := mountedSnapshotFixture(t, volumeID)
	leaveUnsealedWAL(t, cfg, volumeID)

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeInternal, snapshotErrorCode(err))
	requireNoSnapshot(t, vb, snapshotID)
}

// A seal receipt is the plugin's proof the last client's writes reached the
// backend, so local WAL beside it does not block the snapshot.
func TestSnapshotVolumeEngine_SealedLastClientProceeds(t *testing.T) {
	const volumeID, snapshotID = "vol-drainsealed", "snap-drainsealed0001"
	cfg, _, _ := mountedSnapshotFixture(t, volumeID)
	leaveUnsealedWAL(t, cfg, volumeID)
	receipt, err := json.Marshal(sealReceipt{Volume: volumeID, PID: 1, SealedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sealReceiptPath(cfg.BaseDir, volumeID), receipt, 0o600))

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snapshot.State)
	assert.FileExists(t, sealReceiptPath(cfg.BaseDir, volumeID), "the receipt belongs to the unmount it attests to")
}

// A socket path the plugin could not have listened on never appears, so a
// connected client there can never be drained: that is permanent, not retry.
func TestSnapshotVolumeEngine_ConnectedClientWithUnservableSocketFails(t *testing.T) {
	const volumeID, snapshotID = "vol-drainlongpath", "snap-drainlongpath01"
	baseDir := filepath.Join(t.TempDir(), strings.Repeat("d", maxUnixSocketPath))
	cfg, vb := mountedAt(t, volumeID, baseDir)
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = viperblock.ReleaseVolumeLock(lock) })

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.NotErrorIs(t, err, errExportSettling)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeInternal, snapshotErrorCode(err))
	requireNoSnapshot(t, vb, snapshotID)
}

// A plugin that closes the connection without an ack has not confirmed the
// flush, so the snapshot fails.
func TestSnapshotVolumeEngine_DrainClosedWithoutAckFailsTheSnapshot(t *testing.T) {
	const volumeID, snapshotID = "vol-drainnoack", "snap-drainnoack00001"
	cfg, dataDir, vb := mountedSnapshotFixture(t, volumeID)
	testutil.StartDrainSocket(t, dataDir, volumeID, "")

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	requireNoSnapshot(t, vb, snapshotID)
}

// lockProbingS3Host is a backend that refuses every request, recording for
// each whether a new writer could have taken volumeID's lock at that moment.
type lockProbingS3Host struct {
	url      string
	requests atomic.Int32
	lockable atomic.Int32
}

func startLockProbingS3Host(t *testing.T, baseDir, volumeID string) *lockProbingS3Host {
	t.Helper()
	probe := &lockProbingS3Host{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probe.requests.Add(1)
		if lock, err := viperblock.AcquireVolumeLock(baseDir, volumeID); err == nil {
			probe.lockable.Add(1)
			_ = viperblock.ReleaseVolumeLock(lock)
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	probe.url = srv.URL
	return probe
}

// unmountedSnapshotFixture is the volume's own host with nothing registered
// for it, so a snapshot takes the open-an-engine branch.
func unmountedSnapshotFixture(t *testing.T, volumeID string) (*Config, string, *lockProbingS3Host) {
	t.Helper()
	_, natsURL := setupEmbeddedNATS(t)
	dataDir := testutil.SocketTempDir(t)
	baseDir := filepath.Join(dataDir, "viperblock")
	require.NoError(t, os.MkdirAll(baseDir, 0o750))
	backend := startLockProbingS3Host(t, baseDir, volumeID)
	cfg := &Config{
		NodeName:               "node-a",
		BaseDir:                baseDir,
		S3Host:                 backend.url,
		Bucket:                 "test-bucket",
		Region:                 "us-east-1",
		AccessKey:              "test-access-key",
		SecretKey:              "test-secret-key",
		loadStateRetryAttempts: 1,
	}
	installTestVolumeLeases(t, cfg, natsURL)
	return cfg, dataDir, backend
}

// holdLockAsExport stands in for an nbdkit this node runs but never
// registered, as a failed rebuild in recovery leaves behind.
func holdLockAsExport(t *testing.T, cfg *Config, volumeID string) {
	t.Helper()
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = viperblock.ReleaseVolumeLock(lock) })
}

// An unregistered export still takes guest writes, so the snapshot must have
// it drain before an engine reads the backend's checkpoint.
func TestSnapshotVolumeEngine_UnregisteredExportIsDrainedBeforeTheOpen(t *testing.T) {
	const volumeID, snapshotID = "vol-strayserving", "snap-strayserving001"
	cfg, dataDir, backend := unmountedSnapshotFixture(t, volumeID)
	holdLockAsExport(t, cfg, volumeID)
	accepted, release := testutil.StartBlockingDrainSocket(t, dataDir, volumeID, "OK\n")

	done := make(chan error, 1)
	go func() {
		_, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)
		done <- err
	}()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("an unregistered export was never asked to drain")
	}
	assert.Zero(t, backend.requests.Load(), "the engine must not read the backend before the drain acks")
	release()

	select {
	case err := <-done:
		require.Error(t, err, "the refusing backend fails the open")
		assert.NotErrorIs(t, err, errExportDrainFailed)
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot did not proceed after the drain acked")
	}
	assert.Positive(t, backend.requests.Load(), "a drained export lets the engine open")
}

// An unregistered export with no socket cannot be drained, so the snapshot is
// refused as retry-later before any engine reads the backend.
func TestSnapshotVolumeEngine_UnregisteredExportWithoutSocketIsRetryLater(t *testing.T) {
	const volumeID, snapshotID = "vol-straysettle", "snap-straysettle0001"
	cfg, _, backend := unmountedSnapshotFixture(t, volumeID)
	holdLockAsExport(t, cfg, volumeID)

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportSettling)
	assert.Nil(t, snapshot)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, snapshotErrorCode(err))
	assert.Zero(t, backend.requests.Load(), "a refused snapshot must not open an engine")
}

// A departed export that left unsealed WAL holds writes the backend lacks.
func TestSnapshotVolumeEngine_UnmountedUnsealedWALFailsBeforeTheOpen(t *testing.T) {
	const volumeID, snapshotID = "vol-strayunsealed", "snap-strayunsealed01"
	cfg, _, backend := unmountedSnapshotFixture(t, volumeID)
	leaveUnsealedWAL(t, cfg, volumeID)

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	assert.Zero(t, backend.requests.Load(), "a refused snapshot must not open an engine")
}

// With no export to drain, the volume lock is held across the open so a
// writer cannot start between the check and the read.
func TestSnapshotVolumeEngine_UnmountedOpenHoldsTheVolumeLock(t *testing.T) {
	const volumeID, snapshotID = "vol-strayidle", "snap-strayidle000001"
	cfg, _, backend := unmountedSnapshotFixture(t, volumeID)

	_, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.Error(t, err, "the refusing backend fails the open")
	assert.NotErrorIs(t, err, errExportDrainFailed)
	assert.NotErrorIs(t, err, errExportSettling)
	require.Positive(t, backend.requests.Load(), "a node with no export and no WAL opens the engine")
	assert.Zero(t, backend.lockable.Load(), "a writer could take the volume lock while the engine was reading")
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	require.NoError(t, err, "the snapshot must give the lock up when it is done")
	require.NoError(t, viperblock.ReleaseVolumeLock(lock))
}

// writeSealReceiptAt leaves the receipt the plugin writes after a seal at sealedAt.
func writeSealReceiptAt(t *testing.T, cfg *Config, volumeID string, sealedAt time.Time) {
	t.Helper()
	receipt, err := json.Marshal(sealReceipt{Volume: volumeID, PID: 1, SealedAt: sealedAt})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sealReceiptPath(cfg.BaseDir, volumeID), receipt, 0o600))
}

// touchWAL dates every entry of the volume's local WAL to at.
func touchWAL(t *testing.T, cfg *Config, volumeID string, at time.Time) {
	t.Helper()
	dir, err := localWALDir(cfg, volumeID)
	require.NoError(t, err)
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, at, at)
	}))
}

// A receipt is proof only for the WAL that existed when it was written. WAL a
// later client wrote and failed to seal is newer than it, so it is refused.
func TestSnapshotVolumeEngine_SealReceiptOlderThanTheWALFails(t *testing.T) {
	const volumeID, snapshotID = "vol-drainstaleseal", "snap-drainstaleseal1"
	cfg, _, vb := mountedSnapshotFixture(t, volumeID)
	sealedAt := time.Now().Add(-time.Minute)
	writeSealReceiptAt(t, cfg, volumeID, sealedAt)
	leaveUnsealedWAL(t, cfg, volumeID)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.BaseDir, volumeID, "wal", "chunks", "wal.00000002.bin"), []byte("w"), 0o600))
	touchWAL(t, cfg, volumeID, sealedAt.Add(time.Second))

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.ErrorIs(t, err, errExportDrainFailed)
	assert.Nil(t, snapshot)
	requireNoSnapshot(t, vb, snapshotID)
}

// A receipt written after every WAL change attests to all of it.
func TestSnapshotVolumeEngine_SealReceiptNewerThanTheWALProceeds(t *testing.T) {
	const volumeID, snapshotID = "vol-drainfreshseal", "snap-drainfreshseal1"
	cfg, _, _ := mountedSnapshotFixture(t, volumeID)
	leaveUnsealedWAL(t, cfg, volumeID)
	walChanged := time.Now().Add(-time.Minute)
	touchWAL(t, cfg, volumeID, walChanged)
	writeSealReceiptAt(t, cfg, volumeID, walChanged.Add(time.Second))

	snapshot, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snapshot.State)
}

// The plugin dates its receipt to the millisecond, after the seal's own WAL
// changes, so a WAL change inside the receipt's millisecond is not newer.
func TestSnapshotVolumeEngine_SealReceiptInTheWALsMillisecondProceeds(t *testing.T) {
	const volumeID, snapshotID = "vol-drainsamemilli", "snap-drainsamemilli1"
	cfg, _, _ := mountedSnapshotFixture(t, volumeID)
	leaveUnsealedWAL(t, cfg, volumeID)
	sealedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	touchWAL(t, cfg, volumeID, sealedAt.Add(400*time.Microsecond))
	writeSealReceiptAt(t, cfg, volumeID, sealedAt)

	_, err := snapshotVolumeEngine(t.Context(), cfg, volumeID, snapshotID)

	require.NoError(t, err)
}
