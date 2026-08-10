package viperblockd

// Tests for startup recovery: rebuilding cfg.MountedVolumes from nbdkit
// processes that survived a restart, exercised against fabricated
// proc/pidfile directories rather than the real host /proc.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nbdkitArgv builds a realistic nbdkit argv (matching nbd.buildArgs' shape)
// for volume, using either socket or port depending on which is non-zero.
func nbdkitArgv(pidFile, socket string, port int, volume string) []string {
	args := []string{"-f", "--pidfile", pidFile}
	if socket != "" {
		args = append(args, "--unix", socket)
	} else {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args,
		"/plugins/nbdkit-viperblock-plugin.so",
		"size=1073741824",
		"volume="+volume,
		"bucket=test-bucket",
		"region=us-east-1",
		"base_dir=/tmp/vb",
		"host=https://s3.mock.local",
		"cache_size=0",
		"shardwal=false",
		"gc_enabled=false",
	)
	return args
}

// writeProcFixture creates procRoot/<pid>/{comm,cmdline}, mimicking what the
// kernel exposes for a running process, without spawning anything real.
func writeProcFixture(t *testing.T, procRoot string, pid int, comm string, argv []string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0644))
}

// --- argv parsing ---

func TestParseNbdkitCmdline_SocketTransport(t *testing.T) {
	argv := nbdkitArgv("/run/spinifex/nbdkit-vol-vol-argv001.pid", "/run/spinifex/nbd/vol-argv001.sock", 0, "vol-argv001")
	cmdline := []byte(strings.Join(argv, "\x00") + "\x00")

	disc, ok := parseNbdkitCmdline(cmdline)
	require.True(t, ok)
	assert.Equal(t, "vol-argv001", disc.Volume)
	assert.Equal(t, "/run/spinifex/nbd/vol-argv001.sock", disc.Socket)
	assert.Zero(t, disc.Port)
}

func TestParseNbdkitCmdline_TCPTransport(t *testing.T) {
	argv := nbdkitArgv("/run/spinifex/nbdkit-vol-vol-argv002.pid", "", 10812, "vol-argv002")
	cmdline := []byte(strings.Join(argv, "\x00") + "\x00")

	disc, ok := parseNbdkitCmdline(cmdline)
	require.True(t, ok)
	assert.Equal(t, "vol-argv002", disc.Volume)
	assert.Equal(t, 10812, disc.Port)
	assert.Empty(t, disc.Socket)
}

func TestParseNbdkitCmdline_NoVolumeArgument_NotOK(t *testing.T) {
	cmdline := []byte(strings.Join([]string{"-f", "--pidfile", "/tmp/x.pid", "--unix", "/tmp/x.sock", "/plugin.so"}, "\x00") + "\x00")

	_, ok := parseNbdkitCmdline(cmdline)
	assert.False(t, ok, "cmdline with no volume=<id> token must not be treated as one of ours")
}

// --- /proc scanning ---

func TestScanNbdkitProcs_OnlyMatchesNbdkitComm(t *testing.T) {
	procRoot := t.TempDir()

	writeProcFixture(t, procRoot, 100, "nbdkit",
		nbdkitArgv("/run/spinifex/nbdkit-vol-vol-scan001.pid", "/run/spinifex/nbd/vol-scan001.sock", 0, "vol-scan001"))

	// Same volume=<id> shape on argv, but comm is not nbdkit: scanNbdkitProcs
	// must reject this on comm alone, proving cmdline content is not enough.
	writeProcFixture(t, procRoot, 200, "bash",
		nbdkitArgv("/run/spinifex/nbdkit-vol-vol-scan002.pid", "/run/spinifex/nbd/vol-scan002.sock", 0, "vol-scan002"))

	found := scanNbdkitProcs(procRoot)
	require.Len(t, found, 1, "only the nbdkit-comm process may be discovered")
	assert.Equal(t, 100, found[0].PID)
	assert.Equal(t, "vol-scan001", found[0].Volume)
	assert.Equal(t, "/run/spinifex/nbd/vol-scan001.sock", found[0].Socket)
}

// --- pidfile corroboration ---

func TestCorroborateNbdkit_MatchingPidfileAccepted(t *testing.T) {
	pidDir := t.TempDir()
	require.NoError(t, utils.WritePidFileTo(pidDir, "nbdkit-vol-vol-corrob001", 4242))

	disc := discoveredNbdkit{PID: 4242, Volume: "vol-corrob001"}
	assert.True(t, corroborateNbdkit(pidDir, disc))
}

func TestCorroborateNbdkit_DisagreeingPidfileRejected(t *testing.T) {
	pidDir := t.TempDir()
	require.NoError(t, utils.WritePidFileTo(pidDir, "nbdkit-vol-vol-corrob002", 9999))

	// /proc says 4242, but the volume's own pidfile names a different PID: a
	// stale pidfile from an earlier process must not vouch for this one.
	disc := discoveredNbdkit{PID: 4242, Volume: "vol-corrob002"}
	assert.False(t, corroborateNbdkit(pidDir, disc))
}

func TestCorroborateNbdkit_MissingPidfileRejected(t *testing.T) {
	pidDir := t.TempDir()

	disc := discoveredNbdkit{PID: 4242, Volume: "vol-corrob003"}
	assert.False(t, corroborateNbdkit(pidDir, disc))
}

// --- -efi cache sizing shared with mountVolume ---

// TestRebuildMountedVolume_EFIVolumeDisablesCache proves recovery takes the
// same -efi cache-disabled branch mountVolume does (both share
// constructMountedVB), observed via its log line before the forced failure.
func TestRebuildMountedVolume_EFIVolumeDisablesCache(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = fastFailingS3Host(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	logs := captureLogs(t)

	disc := discoveredNbdkit{PID: 4242, Volume: "vol-efirecov001-efi", Socket: "/run/spinifex/nbd/vol-efirecov001-efi.sock"}
	_, err := rebuildMountedVolume(context.Background(), cfg, nc, disc)
	require.Error(t, err, "the fast-failing backend must fail construction, or this test proves nothing about which path it took")

	assert.Contains(t, logs.String(), `msg="Disabling cache for auxiliary volume" volume=vol-efirecov001-efi`,
		"an -efi volume recovered from a surviving nbdkit process must take the cache-disabled branch")
	assert.NotContains(t, logs.String(), "Enabling 128MB cache for main volume")
}

// --- end-to-end: discoverable mount registers and is reused, not reopened ---

// fileBackedConstructVB returns a Config.constructVB override that builds a
// real, file-backed VB (no real predastore needed) and stops its chunk
// uploader, mirroring what constructMountedVB does for production.
func fileBackedConstructVB(t *testing.T) func(ctx context.Context, volumeName string) (*viperblock.VB, int, error) {
	t.Helper()
	return func(_ context.Context, volumeName string) (*viperblock.VB, int, error) {
		vb := createTestVBWithState(t, volumeName)
		vb.StopChunkUploader()
		return vb, 0, nil
	}
}

// TestRecoverMountedVolumes_DiscoverableMount_ResolvesAndSkipsReopen is the
// invariant recovery exists for: findMountedVolume must resolve a recovered
// volume, and a subsequent snapshot must reuse it, never open a second engine.
func TestRecoverMountedVolumes_DiscoverableMount_ResolvesAndSkipsReopen(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.constructVB = fileBackedConstructVB(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-recoverable001"
	const srcSnapshotID = "snap-recoverablesrc1"
	const dstSnapshotID = "snap-recoverabledst1"

	procRoot := t.TempDir()
	pidDir := t.TempDir()
	const pid = 424242
	const socket = "/run/spinifex/nbd/vol-recoverable001.sock"

	writeProcFixture(t, procRoot, pid, "nbdkit",
		nbdkitArgv(filepath.Join(pidDir, "nbdkit-vol-"+volumeName+".pid"), socket, 0, volumeName))
	require.NoError(t, utils.WritePidFileTo(pidDir, "nbdkit-vol-"+volumeName, pid))

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot, pidDir)

	mv, ok := findMountedVolume(cfg, volumeName)
	require.True(t, ok, "recovery must register the surviving nbdkit process in MountedVolumes")
	assert.Equal(t, pid, mv.PID)
	assert.Equal(t, socket, mv.Socket)
	t.Cleanup(func() {
		if mv.VB != nil {
			mv.VB.StopWALSyncer()
		}
	})

	// Capture logs only from here: recovery's own construction legitimately
	// opens the volume once, which is not the invariant under test.
	logs := captureLogs(t)

	wantCompletionSubject, err := ebsprovider.SnapshotCompletionSubject(srcSnapshotID)
	require.NoError(t, err)
	completionSub, err := nc.SubscribeSync(wantCompletionSubject)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	createBody := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"snapshot_id":    srcSnapshotID,
	})
	requestProvider(t, nc, ebsprovider.SnapshotCreateSubjectPrefix+volumeName, createBody)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completionMsg, err := completionSub.NextMsgWithContext(ctx)
	require.NoError(t, err)
	var completed ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(completionMsg.Data, &completed))
	require.Nil(t, completed.Error, "snapshot create on a recovered volume must succeed with no S3 backend involved")

	copyBody := marshalRequest(t, map[string]any{
		"schema_version":          ebsprovider.SchemaVersion,
		"source_snapshot_id":      srcSnapshotID,
		"destination_snapshot_id": dstSnapshotID,
		"volume_id":               volumeName,
	})
	copyMsg := requestProvider(t, nc, ebsprovider.CopySnapshotSubject, copyBody)
	var copyResp ebsprovider.CopySnapshotResponse
	require.NoError(t, json.Unmarshal(copyMsg.Data, &copyResp))
	require.Nil(t, copyResp.Error, "snapshot copy on a recovered volume must succeed with no S3 backend involved")

	assert.Equal(t, 0, volumeOpenCount(logs.String(), volumeName),
		"snapshot create+copy on a volume recovered from a surviving nbdkit process must reuse the live VB, never open a second engine")
}

// TestRecoverMountedVolumes_UncorroboratedProcessSkipped proves recovery
// leaves MountedVolumes empty when a discovered nbdkit process has no
// pidfile vouching for it, rather than trusting /proc's comm alone.
func TestRecoverMountedVolumes_UncorroboratedProcessSkipped(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.constructVB = fileBackedConstructVB(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-uncorroborated1"
	procRoot := t.TempDir()
	pidDir := t.TempDir() // deliberately left empty: no pidfile at all

	writeProcFixture(t, procRoot, 55555, "nbdkit",
		nbdkitArgv(filepath.Join(pidDir, "nbdkit-vol-"+volumeName+".pid"), "/run/spinifex/nbd/vol-uncorroborated1.sock", 0, volumeName))

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot, pidDir)

	_, ok := findMountedVolume(cfg, volumeName)
	assert.False(t, ok, "a process with no corroborating pidfile must never be adopted into MountedVolumes")
}
