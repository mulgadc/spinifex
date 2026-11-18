package viperblock

// Tests for UnpublishVolume's export teardown over NATS

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/deadpid"
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

// --- unpublish socket cleanup ---

func TestIntegration_UnpublishRemovesSocket(t *testing.T) {
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
			PID:    deadpid.PID,
		},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-unmount-socket", 3*time.Second)
	assert.Nil(t, resp.Error)

	// Verify socket file was removed
	assert.False(t, fileExistsCheck(socketPath))
}

// --- unpublish with VB instance ---

func TestIntegration_UnpublishWithVBInstance(t *testing.T) {
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

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:   "vol-unmount-vb",
			Socket: socketPath,
			PID:    deadpid.PID,
			VB:     vb,
		},
	}

	startTestService(t, cfg)

	resp := unpublishOverContract(t, nc, cfg, "vol-unmount-vb", 3*time.Second)
	assert.Nil(t, resp.Error)
	assert.True(t, resp.TornDown)

	// Verify cleanup
	assert.False(t, fileExistsCheck(socketPath))
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	assert.Empty(t, cfg.MountedVolumes, "an unpublished volume must leave MountedVolumes")
}

// fileExistsCheck is a helper to check if a file exists on disk.
func fileExistsCheck(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
