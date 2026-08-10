package viperblockd

// Tests for startup recovery: rebuilding cfg.MountedVolumes from nbdkit
// processes that survived a restart, exercised against a fabricated proc
// directory rather than the real host /proc.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nbdkitArgv builds a realistic nbdkit argv (matching nbd.buildArgs' shape)
// for volume, using either socket or port depending on which is non-zero.
// The --pidfile is present but never written, exactly as nbdkit -f leaves it.
func nbdkitArgv(baseDir, socket string, port int, volume string) []string {
	args := []string{"-f", "--pidfile", "/tmp/nbdkit-vol-" + volume + ".pid"}
	if socket != "" {
		args = append(args, "--unix", socket)
	} else {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args,
		"/plugins/"+vbPluginSuffix,
		"size=1073741824",
		"volume="+volume,
		"bucket=test-bucket",
		"region=us-east-1",
		"base_dir="+baseDir,
		"host=https://s3.mock.local",
		"cache_size=0",
		"shardwal=false",
		"gc_enabled=false",
	)
	return args
}

// listenUnix creates a real listening socket at path, so corroboration's
// "the socket this argv names still exists" check has something to find.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
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
	argv := nbdkitArgv("/var/lib/vb", "/run/spinifex/nbd/vol-argv001.sock", 0, "vol-argv001")
	cmdline := []byte(strings.Join(argv, "\x00") + "\x00")

	disc, ok := parseNbdkitCmdline(cmdline)
	require.True(t, ok)
	assert.Equal(t, "vol-argv001", disc.Volume)
	assert.Equal(t, "/run/spinifex/nbd/vol-argv001.sock", disc.Socket)
	assert.Zero(t, disc.Port)
	assert.Equal(t, "/var/lib/vb", disc.BaseDir)
	assert.Equal(t, "/plugins/"+vbPluginSuffix, disc.Plugin)
}

func TestParseNbdkitCmdline_TCPTransport(t *testing.T) {
	argv := nbdkitArgv("/var/lib/vb", "", 10812, "vol-argv002")
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
		nbdkitArgv("/var/lib/vb", "/run/spinifex/nbd/vol-scan001.sock", 0, "vol-scan001"))

	// Same volume=<id> shape on argv, but comm is not nbdkit: scanNbdkitProcs
	// must reject this on comm alone, proving cmdline content is not enough.
	writeProcFixture(t, procRoot, 200, "bash",
		nbdkitArgv("/var/lib/vb", "/run/spinifex/nbd/vol-scan002.sock", 0, "vol-scan002"))

	found := scanNbdkitProcs(procRoot)
	require.Len(t, found, 1, "only the nbdkit-comm process may be discovered")
	assert.Equal(t, 100, found[0].PID)
	assert.Equal(t, "vol-scan001", found[0].Volume)
	assert.Equal(t, "/run/spinifex/nbd/vol-scan001.sock", found[0].Socket)
}

// --- corroboration ---

// corroborationFixture returns a Config and a discovery that agree on every
// checked field, with a real socket in place, so each test below can spoil
// exactly one of them and attribute the rejection to that field.
func corroborationFixture(t *testing.T) (*Config, discoveredNbdkit) {
	t.Helper()
	baseDir := t.TempDir()
	socket := filepath.Join(t.TempDir(), "vol-corrob.sock")
	listenUnix(t, socket)

	return &Config{BaseDir: baseDir}, discoveredNbdkit{
		PID:     4242,
		Volume:  "vol-corrob001",
		Socket:  socket,
		Plugin:  "/plugins/" + vbPluginSuffix,
		BaseDir: baseDir,
	}
}

func TestCorroborateNbdkit_OurPluginAndBaseDirAccepted(t *testing.T) {
	cfg, disc := corroborationFixture(t)
	assert.True(t, corroborateNbdkit(cfg, disc))
}

func TestCorroborateNbdkit_ForeignPluginRejected(t *testing.T) {
	cfg, disc := corroborationFixture(t)
	disc.Plugin = "/usr/lib/nbdkit/plugins/nbdkit-file-plugin.so"
	assert.False(t, corroborateNbdkit(cfg, disc), "an nbdkit serving someone else's plugin is not ours to adopt")
}

func TestCorroborateNbdkit_ForeignBaseDirRejected(t *testing.T) {
	cfg, disc := corroborationFixture(t)
	disc.BaseDir = filepath.Join(t.TempDir(), "other")
	assert.False(t, corroborateNbdkit(cfg, disc), "an nbdkit for a different data dir belongs to a different daemon")
}

func TestCorroborateNbdkit_MissingSocketRejected(t *testing.T) {
	cfg, disc := corroborationFixture(t)
	disc.Socket = filepath.Join(t.TempDir(), "gone.sock")
	assert.False(t, corroborateNbdkit(cfg, disc), "a socket mount whose socket is gone is not still serving")
}

func TestCorroborateNbdkit_TCPMountAcceptedOnPort(t *testing.T) {
	cfg, disc := corroborationFixture(t)
	disc.Socket = ""
	disc.Port = 10812
	assert.True(t, corroborateNbdkit(cfg, disc), "a TCP mount leaves no filesystem trace, so the port is all there is")
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

	disc := discoveredNbdkit{PID: 4242, Volume: "vol-efirecov001-efi", Socket: filepath.Join(t.TempDir(), "efi.sock")}
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
	const pid = 424242
	socket := filepath.Join(t.TempDir(), "vol-recoverable001.sock")
	listenUnix(t, socket)

	writeProcFixture(t, procRoot, pid, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

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
// leaves MountedVolumes empty for an nbdkit belonging to a different daemon,
// rather than adopting anything whose comm and volume= argument look right.
func TestRecoverMountedVolumes_UncorroboratedProcessSkipped(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.constructVB = fileBackedConstructVB(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-uncorroborated1"
	procRoot := t.TempDir()
	socket := filepath.Join(t.TempDir(), "vol-uncorroborated1.sock")
	listenUnix(t, socket)

	writeProcFixture(t, procRoot, 55555, "nbdkit",
		nbdkitArgv(filepath.Join(t.TempDir(), "someone-elses-data-dir"), socket, 0, volumeName))

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

	_, ok := findMountedVolume(cfg, volumeName)
	assert.False(t, ok, "an nbdkit serving another daemon's base dir must never be adopted into MountedVolumes")
}

// TestRecoverMountedVolumes_DuplicateVolumeAdoptedOnce proves two live nbdkits
// for one volume — the double-mount hazard itself — yield a single registry
// entry rather than two conflicting ones.
func TestRecoverMountedVolumes_DuplicateVolumeAdoptedOnce(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.constructVB = fileBackedConstructVB(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-duplicatemount1"
	procRoot := t.TempDir()
	socketDir := t.TempDir()

	for i, pid := range []int{6001, 6002} {
		socket := filepath.Join(socketDir, fmt.Sprintf("dup-%d.sock", i))
		listenUnix(t, socket)
		writeProcFixture(t, procRoot, pid, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))
	}

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

	count := 0
	for _, mv := range cfg.MountedVolumes {
		if mv.Name == volumeName {
			count++
			t.Cleanup(func() { mv.VB.StopWALSyncer() })
		}
	}
	assert.Equal(t, 1, count, "two nbdkits for one volume must produce one registry entry, not two")
}
