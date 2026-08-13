// This test proves the qemu-nbd EBS provider works end to end against a
// real qemunbdd daemon over a live cluster's NATS. It is opt-in only: run it
// with SPINIFEX_QEMUNBD_LIVE=1 on the node the daemon is running on.
package qemunbdd_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/require"
)

// liveEnvVar opts this test into running against a real cluster; unset (the
// default) it always skips, so it never runs under make preflight.
const liveEnvVar = "SPINIFEX_QEMUNBD_LIVE"

// defaultConfigPath mirrors the path the qemunbd systemd unit sets for
// SPINIFEX_CONFIG_PATH when the operator does not override it.
const defaultConfigPath = "/etc/spinifex/spinifex.toml"

const liveVolumeBytes int64 = 64 * 1024 * 1024

// TestLive_QEMUNBDLifecycle drives a real qemunbdd daemon end to end through
// ebsprovider.NewNATSProvider, the same client shape the control plane uses,
// over the cluster's real NATS.
func TestLive_QEMUNBDLifecycle(t *testing.T) {
	if os.Getenv(liveEnvVar) == "" {
		t.Skipf("skipping: set %s=1 to run this test against a real qemunbdd daemon over a live cluster's NATS", liveEnvVar)
	}

	cfgPath := os.Getenv("SPINIFEX_CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = defaultConfigPath
	}
	t.Logf("loading cluster config from %s", cfgPath)

	clusterConfig, err := config.LoadConfig(cfgPath)
	require.NoError(t, err, "load cluster config")

	nodeID := clusterConfig.Node
	require.NotEmpty(t, nodeID, "cluster config has no node name set")

	nodeConfig, ok := clusterConfig.Nodes[nodeID]
	require.True(t, ok, "cluster config has no entry for node %q", nodeID)

	t.Logf("node %q: base dir %s", nodeID, nodeConfig.Predastore.BaseDir)

	// NATS token and CA cert are read from config and handed straight to the
	// connect helper. Never log them, and never let a failure message embed
	// the config value that produced it.
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(nodeConfig.NATS.Host), nodeConfig.NATS.ACL.Token, nodeConfig.NATS.CACert)
	require.NoError(t, err, "connect to NATS")
	t.Cleanup(nc.Close)
	t.Logf("connected to NATS")

	provider := ebsprovider.NewNATSProvider(nc, 60*time.Second)

	runID := strconv.FormatInt(time.Now().UnixNano(), 10)
	volumeID := "live-qnbd-vol-" + runID
	snapshotID := "live-qnbd-snap-" + runID
	restoredVolumeID := "live-qnbd-restore-" + runID
	pattern := byte(0xAB)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupBestEffort(t, "unpublish restored volume", func() error {
			return provider.UnpublishVolume(cleanupCtx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: restoredVolumeID, NodeID: nodeID})
		})
		cleanupBestEffort(t, "delete restored volume", func() error {
			return provider.DeleteVolume(cleanupCtx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: restoredVolumeID})
		})
		cleanupBestEffort(t, "unpublish volume", func() error {
			return provider.UnpublishVolume(cleanupCtx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: nodeID})
		})
		cleanupBestEffort(t, "delete snapshot", func() error {
			return provider.DeleteSnapshot(cleanupCtx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID})
		})
		cleanupBestEffort(t, "delete volume", func() error {
			return provider.DeleteVolume(cleanupCtx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID})
		})
	})

	ctx := t.Context()

	vol, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: liveVolumeBytes},
	})
	require.NoError(t, err, "create volume")
	t.Logf("created volume %s: %d bytes, state=%s", vol.ID, vol.CapacityBytes, vol.State)

	pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: nodeID})
	require.NoError(t, err, "publish volume")
	t.Logf("published volume %s to node %s: %s", pub.VolumeID, pub.NodeID, pub.NBDURI)
	sockPath := requireUnixSocketPath(t, pub.NBDURI)

	writePattern(t, sockPath, pattern, liveVolumeBytes)
	readAndVerifyPattern(t, sockPath, pattern, liveVolumeBytes)

	require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: nodeID}), "unpublish volume")
	t.Logf("unpublished volume %s", volumeID)

	snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID})
	require.NoError(t, err, "create snapshot")
	t.Logf("created snapshot %s from volume %s: state=%s, size=%d", snap.ID, volumeID, snap.State, snap.SizeBytes)
	require.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State, "snapshot must report completed")

	restoredVol, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:        ebsprovider.NewVersioned(),
		VolumeID:         restoredVolumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: liveVolumeBytes},
		SourceSnapshotID: snapshotID,
	})
	require.NoError(t, err, "create volume from snapshot")
	t.Logf("created volume %s from snapshot %s: %d bytes, state=%s", restoredVol.ID, snapshotID, restoredVol.CapacityBytes, restoredVol.State)

	restoredPub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: restoredVolumeID, NodeID: nodeID})
	require.NoError(t, err, "publish restored volume")
	t.Logf("published restored volume %s to node %s: %s", restoredPub.VolumeID, restoredPub.NodeID, restoredPub.NBDURI)
	restoredSockPath := requireUnixSocketPath(t, restoredPub.NBDURI)

	// The real proof: bytes written before the snapshot survive both the
	// snapshot and the restore into an independent volume.
	readAndVerifyPattern(t, restoredSockPath, pattern, liveVolumeBytes)

	require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: restoredVolumeID, NodeID: nodeID}), "unpublish restored volume")
	t.Logf("unpublished restored volume %s", restoredVolumeID)
}

// cleanupBestEffort logs a cleanup step's outcome without failing the test:
// cleanup runs after the real assertions and must never mask their result.
func cleanupBestEffort(t *testing.T, step string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Logf("cleanup: %s: %v", step, err)
	} else {
		t.Logf("cleanup: %s: ok", step)
	}
}

// requireUnixSocketPath asserts nbdURI is a nbd:unix: URI and returns its
// socket path, parsed with the same helper the daemon uses to format it
// rather than by slicing the string.
func requireUnixSocketPath(t *testing.T, nbdURI string) string {
	t.Helper()
	serverType, path, _, _, err := utils.ParseNBDURI(nbdURI)
	require.NoError(t, err, "parse NBD URI %q", nbdURI)
	require.Equal(t, "unix", serverType, "published volume must be a nbd:unix: URI, got %q", nbdURI)
	require.NotEmpty(t, path, "NBD URI %q has an empty socket path", nbdURI)
	return path
}

// qemuIOTarget builds the client-facing NBD URI qemu-io understands.
// qemu-nbd exports the guest view raw regardless of the qcow2 file backing
// it, so the client always dials with -f raw.
func qemuIOTarget(sockPath string) string {
	return fmt.Sprintf("nbd+unix:///?socket=%s", sockPath)
}

// runQEMUIO execs one qemu-io command against sockPath as its own process,
// which is itself a fresh NBD connection independent of any prior exec.
func runQEMUIO(t *testing.T, sockPath, ioCmd string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "qemu-io", "-f", "raw", "-c", ioCmd, qemuIOTarget(sockPath)).CombinedOutput()
	return string(out), err
}

// writePattern writes a byte pattern across the whole volume through NBD.
func writePattern(t *testing.T, sockPath string, pattern byte, size int64) {
	t.Helper()
	ioCmd := fmt.Sprintf("write -P 0x%02x 0 %d", pattern, size)
	out, err := runQEMUIO(t, sockPath, ioCmd)
	require.NoError(t, err, "qemu-io write failed: %s", strings.TrimSpace(out))
	t.Logf("wrote pattern 0x%02x across %d bytes via qemu-io", pattern, size)
}

// readAndVerifyPattern reads the volume back through NBD and asserts every
// byte matches pattern, checking both qemu-io's exit status and its output
// for a mismatch complaint.
func readAndVerifyPattern(t *testing.T, sockPath string, pattern byte, size int64) {
	t.Helper()
	ioCmd := fmt.Sprintf("read -P 0x%02x 0 %d", pattern, size)
	out, err := runQEMUIO(t, sockPath, ioCmd)
	require.NoError(t, err, "qemu-io read failed: %s", strings.TrimSpace(out))
	require.NotContains(t, out, "Pattern verification failed", "qemu-io reported a pattern mismatch: %s", strings.TrimSpace(out))
	t.Logf("verified pattern 0x%02x across %d bytes via qemu-io", pattern, size)
}
