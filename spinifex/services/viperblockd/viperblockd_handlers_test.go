package viperblockd

// Tests for NATS message handlers: ebs.delete, ebs.sync, ebs.unmount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/file"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestVBWithState creates a real *viperblock.VB with file backend for handler tests.
func createTestVBWithState(t *testing.T, volumeName string) *viperblock.VB {
	t.Helper()
	baseDir := t.TempDir()
	fileCfg := file.FileConfig{VolumeName: volumeName, BaseDir: baseDir}
	vbCfg := &viperblock.VB{
		VolumeName:      volumeName,
		VolumeSize:      1, // minimal
		BaseDir:         baseDir,
		WALSyncInterval: -1, // disable background goroutine
		Cache:           viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
	}
	vb, err := viperblock.New(vbCfg, "file", fileCfg)
	require.NoError(t, err)
	require.NoError(t, vb.Backend.Init())
	require.NoError(t, vb.SaveState())
	return vb
}

// --- ebs.delete handler tests ---

func TestIntegration_EBSDeleteMountedVolume(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create a temp socket file to verify cleanup
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "vol-del-test.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake-socket"), 0600))

	// Connect a client and create a subscription to use as SnapshotSub
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	configSub, err := nc.Subscribe("ebs.config.vol-del-test", func(msg *nats.Msg) {})
	require.NoError(t, err)

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:      "vol-del-test",
			Port:      10809,
			Socket:    socketPath,
			PID:       99999,
			ConfigSub: configSub,
		},
	}

	startTestService(t, cfg)

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: "vol-del-test"})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))

	assert.Equal(t, "vol-del-test", resp.Volume)
	assert.True(t, resp.Success)
	assert.Empty(t, resp.Error)

	// Verify volume removed from config
	cfg.mu.Lock()
	assert.Empty(t, cfg.MountedVolumes)
	cfg.mu.Unlock()

	// Verify socket file deleted
	assert.False(t, fileExistsCheck(socketPath))

	// Verify config subscription was unsubscribed
	assert.False(t, configSub.IsValid())
}

func TestIntegration_EBSDeleteUnmountedVolume(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	// No mounted volumes

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: "vol-not-mounted"})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
}

// TestIntegration_EBSDeleteRemovesLocalVolumeDirectory covers the residual
// leak: terminateCleanup always unmounts before DeleteVolumes sends
// ebs.delete, so the common case is an already-unmounted volume. Before the
// fix, ebs.delete only cleaned up a still-mounted volume's nbdkit
// process/socket and never touched cfg.BaseDir/<volume>, so the
// WAL/checkpoint directory a prior mount (or a failed unmount seal) left
// behind survived every DeleteVolume call — including for -efi companion
// volumes, which never go through the unmount seal at all (isAuxVolume skips
// it), so they leaked unconditionally rather than only on a failed seal.
func TestIntegration_EBSDeleteRemovesLocalVolumeDirectory(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	// No mounted volumes: matches the terminate path where Unmount already
	// ran and left local state behind (a failed seal for the main volume, or
	// unconditionally for the -efi companion) before ebs.delete fires.
	createMockVolumeState(t, cfg.BaseDir, "vol-residual-test")
	createMockVolumeState(t, cfg.BaseDir, "vol-residual-test-efi")

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	for _, volume := range []string{"vol-residual-test", "vol-residual-test-efi"} {
		reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: volume})
		msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
		require.NoError(t, err)

		var resp types.EBSDeleteResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		assert.True(t, resp.Success)

		assert.False(t, fileExistsCheck(filepath.Join(cfg.BaseDir, volume)),
			"ebs.delete must remove the local volume directory for %s", volume)
	}
}

// TestIntegration_EBSDeleteEmptyVolumeNameDoesNotWipeBaseDir is the
// regression test for the data-destruction hazard: before validation,
// filepath.Join(cfg.BaseDir, "") collapsed to cfg.BaseDir itself, so an empty
// Volume made ebs.delete os.RemoveAll the node's entire local WAL/checkpoint
// cache. This must be rejected, and BaseDir must survive.
func TestIntegration_EBSDeleteEmptyVolumeNameDoesNotWipeBaseDir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	createMockVolumeState(t, cfg.BaseDir, "vol-survivor")

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: ""})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Success, "an empty volume name must not report success")
	assert.NotEmpty(t, resp.Error)

	assert.True(t, fileExistsCheck(cfg.BaseDir), "BaseDir itself must survive an empty volume name")
	assert.True(t, fileExistsCheck(filepath.Join(cfg.BaseDir, "vol-survivor")),
		"an unrelated volume directory must survive an empty volume name")
}

// TestIntegration_EBSDeleteRejectsPathTraversal covers names that would
// otherwise let ebs.delete escape BaseDir via filepath.Join's ".." cleaning.
// Both cases must be refused before any RemoveAll runs, and Success must not
// come back true for a request that was refused.
func TestIntegration_EBSDeleteRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	createMockVolumeState(t, cfg.BaseDir, "vol-survivor")

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	for _, name := range []string{"../..", "a/b"} {
		reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: name})
		msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
		require.NoError(t, err)

		var resp types.EBSDeleteResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		assert.False(t, resp.Success, "volume name %q must not report success", name)
		assert.NotEmpty(t, resp.Error, "volume name %q must return an error", name)
	}

	assert.True(t, fileExistsCheck(filepath.Join(cfg.BaseDir, "vol-survivor")),
		"an unrelated volume directory must survive a path-traversal attempt")
}

// TestIntegration_EBSDeleteValidNameLeavesSiblingsUntouched is the essential
// non-regression check for the new validation: a legitimate delete must still
// remove exactly its own directory, and nothing else under BaseDir.
func TestIntegration_EBSDeleteValidNameLeavesSiblingsUntouched(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	createMockVolumeState(t, cfg.BaseDir, "vol-target")
	createMockVolumeState(t, cfg.BaseDir, "vol-sibling")

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: "vol-target"})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
	assert.Empty(t, resp.Error)

	assert.False(t, fileExistsCheck(filepath.Join(cfg.BaseDir, "vol-target")),
		"the targeted volume directory must be removed")
	assert.True(t, fileExistsCheck(filepath.Join(cfg.BaseDir, "vol-sibling")),
		"a sibling volume directory must survive deleting a different volume")
}

func TestIntegration_EBSDeleteInvalidJSON(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	msg, err := nc.Request("ebs.delete", []byte("not json {{{"), 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Contains(t, resp.Error, "bad request:")
}

// --- ebs.sync handler tests ---

func TestIntegration_EBSSyncVolumeNotMounted(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSSyncRequest{Volume: "vol-not-here"})
	msg, err := nc.Request("ebs.sync", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSSyncResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Synced)
	assert.Contains(t, resp.Error, "not mounted")
}

func TestIntegration_EBSSyncVolumeNoVBInstance(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-no-vb", VB: nil}, // Volume exists but no VB instance
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSSyncRequest{Volume: "vol-no-vb"})
	msg, err := nc.Request("ebs.sync", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSSyncResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.False(t, resp.Synced)
	assert.Contains(t, resp.Error, "not mounted")
}

func TestIntegration_EBSSyncInvalidJSON(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	msg, err := nc.Request("ebs.sync", []byte("garbage"), 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSSyncResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Contains(t, resp.Error, "bad request:")
}

// --- ebs.unmount socket cleanup ---

func TestIntegration_EBSUnmountRemovesSocket(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create an actual temp file as the socket
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "vol-unmount-socket.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake"), 0600))

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:   "vol-unmount-socket",
			Socket: socketPath,
			PID:    99999,
		},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSRequest{Name: "vol-unmount-socket"})
	msg, err := nc.Request("ebs.test-node.unmount", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Empty(t, resp.Error)

	// Verify socket file was removed
	assert.False(t, fileExistsCheck(socketPath))
}

// --- ebs.delete removes socket ---

func TestIntegration_EBSDeleteRemovesSocket(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "vol-del-socket.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake"), 0600))
	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-del-socket", Socket: socketPath, PID: 99999},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: "vol-del-socket"})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
	assert.False(t, fileExistsCheck(socketPath))
}

// --- ebs.sync with VB instance ---

func TestIntegration_EBSSyncWithVBInstance(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	vb := createTestVBWithState(t, "vol-sync-vb")

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-sync-vb", VB: vb},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	reqData, _ := json.Marshal(types.EBSSyncRequest{Volume: "vol-sync-vb"})
	msg, err := nc.Request("ebs.sync", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSSyncResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Synced)
	assert.Empty(t, resp.Error)
}

// --- ebs.unmount invalid JSON ---

func TestIntegration_EBSUnmountInvalidJSON(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Handler responds with an error payload on invalid JSON
	msg, err := nc.Request("ebs.test-node.unmount", []byte("not json {{{"), 1*time.Second)
	require.NoError(t, err, "handler should respond even on invalid JSON")

	var resp types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Contains(t, resp.Error, "bad request")
}

// --- ebs.delete with VB instance (StopWALSyncer path) ---

func TestIntegration_EBSDeleteWithVBInstance(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	vb := createTestVBWithState(t, "vol-del-vb")

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "vol-del-vb.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake"), 0600))

	configSub, err := nc.Subscribe("ebs.config.vol-del-vb", func(msg *nats.Msg) {})
	require.NoError(t, err)

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:      "vol-del-vb",
			Socket:    socketPath,
			PID:       99999,
			VB:        vb,
			ConfigSub: configSub,
		},
	}

	startTestService(t, cfg)

	reqData, _ := json.Marshal(types.EBSDeleteRequest{Volume: "vol-del-vb"})
	msg, err := nc.Request("ebs.delete", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSDeleteResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.True(t, resp.Success)
	assert.Empty(t, resp.Error)

	// Verify full cleanup
	assert.False(t, fileExistsCheck(socketPath))
	assert.False(t, configSub.IsValid())
}

// --- ebs.unmount with VB instance + ConfigSub ---

func TestIntegration_EBSUnmountWithVBInstance(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	vb := createTestVBWithState(t, "vol-unmount-vb")

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "vol-unmount-vb.sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake"), 0600))

	configSub, err := nc.Subscribe("ebs.config.vol-unmount-vb", func(msg *nats.Msg) {})
	require.NoError(t, err)

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:      "vol-unmount-vb",
			Socket:    socketPath,
			PID:       99999,
			VB:        vb,
			ConfigSub: configSub,
		},
	}

	startTestService(t, cfg)

	reqData, _ := json.Marshal(types.EBSRequest{Name: "vol-unmount-vb"})
	msg, err := nc.Request("ebs.test-node.unmount", reqData, 3*time.Second)
	require.NoError(t, err)

	var resp types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Equal(t, "vol-unmount-vb", resp.Volume)
	assert.False(t, resp.Mounted)
	assert.Empty(t, resp.Error)

	// Verify cleanup
	assert.False(t, fileExistsCheck(socketPath))
	assert.False(t, configSub.IsValid())
}

// --- ebs.unmount dual-publish verification ---

func TestIntegration_EBSUnmountDualPublish(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-dual-pub", PID: 99999},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Subscribe to the broadcast response topic before sending unmount
	broadcastCh := make(chan *nats.Msg, 1)
	_, err = nc.Subscribe("ebs.unmount.response", func(msg *nats.Msg) {
		broadcastCh <- msg
	})
	require.NoError(t, err)
	nc.Flush()

	reqData, _ := json.Marshal(types.EBSRequest{Name: "vol-dual-pub"})
	msg, err := nc.Request("ebs.test-node.unmount", reqData, 3*time.Second)
	require.NoError(t, err)

	// Verify direct reply
	var directResp types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &directResp))
	assert.Equal(t, "vol-dual-pub", directResp.Volume)
	assert.False(t, directResp.Mounted)

	// Verify broadcast response received
	select {
	case broadcastMsg := <-broadcastCh:
		var broadcastResp types.EBSUnMountResponse
		require.NoError(t, json.Unmarshal(broadcastMsg.Data, &broadcastResp))
		assert.Equal(t, "vol-dual-pub", broadcastResp.Volume)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for broadcast on ebs.unmount.response")
	}
}

// fileExistsCheck is a helper to check if a file exists on disk.
func fileExistsCheck(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
