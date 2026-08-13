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
	"os/exec"
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

// spawnLongRunningProcess starts a real, short-lived-by-signal process so
// reap tests can send it a genuine SIGTERM and observe a genuine exit,
// instead of pretending a fabricated /proc entry is a live process (SIGTERM
// only ever reaches the real kernel process table, never procRoot).
func spawnLongRunningProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return pid
}

// writeReferencerFixture writes a /proc/<pid>/{comm,cmdline} pair for a
// process whose argv carries volumeSocket's endpoint the way QEMU's -blockdev
// does, so endpointReferencer has something positive to find. comm is
// cosmetic only -- endpointReferencer does not filter by it, matching the
// "any process on the host may be a legitimate referencer" fail-safe design.
func writeReferencerFixture(t *testing.T, procRoot string, pid int, blockdevArg string) {
	t.Helper()
	writeProcFixture(t, procRoot, pid, "qemu-system-x86_64", []string{"qemu-system-x86_64", "-blockdev", blockdevArg})
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

// --- orphan reaping ---

// TestReapOrphanedNbdkit_NoReferencer_SignalsAndRemovesSocket proves the
// baseline case the bead asks for: an unclaimed nbdkit with nothing on the
// host referencing its endpoint is signalled and its socket cleaned up.
func TestReapOrphanedNbdkit_NoReferencer_SignalsAndRemovesSocket(t *testing.T) {
	pid := spawnLongRunningProcess(t)
	socket := filepath.Join(t.TempDir(), "vol-reap-unreferenced.sock")
	listenUnix(t, socket)
	procRoot := t.TempDir() // no other processes at all: nothing to find

	logs := captureLogs(t)
	disc := discoveredNbdkit{PID: pid, Volume: "vol-reap-unreferenced1", Socket: socket}
	reapOrphanedNbdkit(procRoot, disc, reapReasonUnadoptable)

	require.Eventually(t, func() bool { return !utils.ProcessAlive(pid) }, 2*time.Second, 20*time.Millisecond,
		"an unreferenced orphan must be signalled and exit")
	_, err := os.Stat(socket)
	assert.True(t, os.IsNotExist(err), "the reaped orphan's socket file must be removed")
	assert.Contains(t, logs.String(), "reaped unclaimed nbdkit process")
	assert.Contains(t, logs.String(), fmt.Sprintf("pid=%d", pid))
	assert.Contains(t, logs.String(), "volume=vol-reap-unreferenced1")
	assert.Contains(t, logs.String(), "reason=unadoptable")
}

// TestReapOrphanedNbdkit_LiveReferencer_LeftRunningAndLogged proves the
// safety rule: an unclaimed nbdkit whose endpoint is still named on another
// process's cmdline must never be signalled, however unadoptable it looked.
func TestReapOrphanedNbdkit_LiveReferencer_LeftRunningAndLogged(t *testing.T) {
	pid := spawnLongRunningProcess(t)
	socket := filepath.Join(t.TempDir(), "vol-reap-referenced.sock")
	listenUnix(t, socket)

	procRoot := t.TempDir()
	const referencerPID = 909090
	writeReferencerFixture(t, procRoot, referencerPID,
		fmt.Sprintf("driver=nbd,node-name=vol0,server.type=unix,server.path=%s,export=", socket))

	logs := captureLogs(t)
	disc := discoveredNbdkit{PID: pid, Volume: "vol-reap-referenced1", Socket: socket}
	reapOrphanedNbdkit(procRoot, disc, reapReasonDuplicate)

	// Give a real signal every chance to have landed before asserting it did not.
	time.Sleep(100 * time.Millisecond)
	assert.True(t, utils.ProcessAlive(pid), "a referenced orphan must never be signalled")
	_, err := os.Stat(socket)
	assert.NoError(t, err, "a live orphan's socket must not be removed")
	assert.Contains(t, logs.String(), "left running deliberately")
	assert.Contains(t, logs.String(), fmt.Sprintf("pid=%d", pid))
	assert.Contains(t, logs.String(), "volume=vol-reap-referenced1")
	assert.Contains(t, logs.String(), fmt.Sprintf("referencing_pid=%d", referencerPID))
}

// TestReapOrphanedNbdkit_TCPEndpointReferencedAcrossSplitBlockdevArgs proves
// endpointReferencer recognizes a TCP endpoint even when QEMU's -blockdev
// splits host and port into server.host=/server.port= within one argument,
// not just the joined host:port form a boot/EFI drive uses.
func TestReapOrphanedNbdkit_TCPEndpointReferencedAcrossSplitBlockdevArgs(t *testing.T) {
	pid := spawnLongRunningProcess(t)
	procRoot := t.TempDir()
	const referencerPID = 909091
	writeReferencerFixture(t, procRoot, referencerPID,
		"driver=nbd,node-name=vol0,server.type=inet,server.host=127.0.0.1,server.port=10899,export=")

	disc := discoveredNbdkit{PID: pid, Volume: "vol-reap-tcp1", Port: 10899}
	reapOrphanedNbdkit(procRoot, disc, reapReasonUnadoptable)

	time.Sleep(100 * time.Millisecond)
	assert.True(t, utils.ProcessAlive(pid), "a TCP endpoint split across server.host=/server.port= must still be recognized as referenced")
}

// unreadableProcDir creates procRoot/<pid> with a cmdline file inside, then
// strips all permissions from the directory itself, so os.ReadFile on the
// cmdline path inside it fails with a permission error rather than
// os.ErrNotExist -- standing in for an /proc mounted with hidepid, where a
// guest owned by another uid is invisible to viperblockd entirely. Skips the
// calling test when running as root, since root ignores directory permission
// bits and the failure this simulates would not occur.
func unreadableProcDir(t *testing.T, procRoot string, pid int) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("cannot simulate a permission-denied /proc entry while running as root")
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte("x\x00"), 0644))
	require.NoError(t, os.Chmod(dir, 0000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) }) // let t.TempDir() clean up
}

// TestEndpointReferencer_MissingCandidateCmdline_NotAnError proves a
// candidate that exited between ReadDir and the cmdline read (ENOENT) is
// correctly read as "not a referencer", not as an inconclusive scan.
func TestEndpointReferencer_MissingCandidateCmdline_NotAnError(t *testing.T) {
	procRoot := t.TempDir()
	// A numbered entry with no cmdline file at all: the process vanished
	// between the directory listing and this read.
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "424243"), 0755))

	disc := discoveredNbdkit{PID: 424242, Volume: "vol-enoent1", Socket: filepath.Join(t.TempDir(), "x.sock")}
	pid, found, err := endpointReferencer(procRoot, disc)

	require.NoError(t, err, "a vanished candidate must not make the scan inconclusive")
	assert.False(t, found)
	assert.Zero(t, pid)
}

// TestEndpointReferencer_UnreadableCandidate_ReturnsError proves the hole the
// safety argument depends on is closed: a candidate this scan could not read
// for any reason other than ENOENT must surface as an error, not as "no
// referencer found", since a read failure is not evidence of absence.
func TestEndpointReferencer_UnreadableCandidate_ReturnsError(t *testing.T) {
	procRoot := t.TempDir()
	unreadableProcDir(t, procRoot, 555555)

	disc := discoveredNbdkit{PID: 424242, Volume: "vol-eacces1", Socket: filepath.Join(t.TempDir(), "x.sock")}
	_, found, err := endpointReferencer(procRoot, disc)

	require.Error(t, err, "a permission-denied candidate must make the scan inconclusive, not silently pass")
	assert.False(t, os.IsNotExist(err), "the error must not be mistaken for ENOENT")
	assert.False(t, found)
}

// TestReapOrphanedNbdkit_UnreadableCandidate_LeftRunningAndLogged is the
// end-to-end version of the fix: an unclaimed nbdkit is left running, not
// reaped, when the referencer scan itself could not be completed -- the same
// fail-safe outcome as an unreadable procRoot, just triggered by one
// unreadable candidate entry among otherwise-readable ones.
func TestReapOrphanedNbdkit_UnreadableCandidate_LeftRunningAndLogged(t *testing.T) {
	pid := spawnLongRunningProcess(t)
	socket := filepath.Join(t.TempDir(), "vol-reap-eacces.sock")
	listenUnix(t, socket)

	procRoot := t.TempDir()
	unreadableProcDir(t, procRoot, 555556)

	logs := captureLogs(t)
	disc := discoveredNbdkit{PID: pid, Volume: "vol-reap-eacces1", Socket: socket}
	reapOrphanedNbdkit(procRoot, disc, reapReasonUnadoptable)

	time.Sleep(100 * time.Millisecond)
	assert.True(t, utils.ProcessAlive(pid), "an inconclusive scan must never result in a signal")
	_, err := os.Stat(socket)
	assert.NoError(t, err, "an orphan left running because the scan was inconclusive must keep its socket")
	assert.Contains(t, logs.String(), "could not scan for processes")
	assert.Contains(t, logs.String(), fmt.Sprintf("pid=%d", pid))
	assert.Contains(t, logs.String(), "volume=vol-reap-eacces1")
}

// TestRecoverMountedVolumes_DuplicateVolume_UnusedDuplicateReaped extends
// TestRecoverMountedVolumes_DuplicateVolumeAdoptedOnce: the duplicate
// recovery declines to adopt must not just be skipped but reaped, while the
// adopted process is never touched, matching the bead's acceptance that no
// volume is ever served by two processes.
func TestRecoverMountedVolumes_DuplicateVolume_UnusedDuplicateReaped(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.constructVB = fileBackedConstructVB(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-duplicatereap1"
	procRoot := t.TempDir()
	socketDir := t.TempDir()

	// Both candidates are real processes: scanNbdkitProcs' adoption order
	// follows os.ReadDir's lexical sort of the PID directory names, which is
	// not something this test controls, so either could end up "adopted".
	// Using two real spawned PIDs means whichever one recovery declines to
	// adopt is always safe to actually SIGTERM.
	pids := map[int]string{}
	for _, name := range []string{"a", "b"} {
		pid := spawnLongRunningProcess(t)
		socket := filepath.Join(socketDir, name+".sock")
		listenUnix(t, socket)
		writeProcFixture(t, procRoot, pid, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))
		pids[pid] = socket
	}

	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

	mv, ok := findMountedVolume(cfg, volumeName)
	require.True(t, ok)
	t.Cleanup(func() { mv.VB.StopWALSyncer() })
	require.Contains(t, pids, mv.PID, "the adopted PID must be one of the two candidates")

	var duplicatePID int
	var duplicateSocket string
	for pid, socket := range pids {
		if pid != mv.PID {
			duplicatePID, duplicateSocket = pid, socket
		}
	}

	require.Eventually(t, func() bool { return !utils.ProcessAlive(duplicatePID) }, 2*time.Second, 20*time.Millisecond,
		"the unused duplicate must be reaped")
	assert.True(t, utils.ProcessAlive(mv.PID), "the adopted process must never be signalled")
	_, err := os.Stat(duplicateSocket)
	assert.True(t, os.IsNotExist(err), "the reaped duplicate's socket file must be removed")
}

// TestRecoverMountedVolumes_UnadoptableReapedAcrossRestarts covers the
// restart-then-stop sequence the bead asks for, not a single mount/unmount
// lifetime: a backend recovery cannot adopt (its volume no longer exists) is
// reaped on the first restart's recovery pass, and a second pass -- standing
// in for the field case's further stop/start cycles -- finds nothing left to
// reap or log, proving the orphan does not survive the way it did before
// this fix.
func TestRecoverMountedVolumes_UnadoptableReapedAcrossRestarts(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = fastFailingS3Host(t) // forces buildVB to fail: the volume is unadoptable
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-unadoptable-restart1"
	procRoot := t.TempDir()
	pid := spawnLongRunningProcess(t)
	socket := filepath.Join(t.TempDir(), "o.sock") // short: unix socket paths cap at ~108 bytes
	listenUnix(t, socket)
	writeProcFixture(t, procRoot, pid, "nbdkit", nbdkitArgv(cfg.BaseDir, socket, 0, volumeName))

	// First restart's recovery pass: the volume cannot be rebuilt, and
	// nothing references its endpoint, so it must be reaped.
	logs := captureLogs(t)
	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

	require.Eventually(t, func() bool { return !utils.ProcessAlive(pid) }, 2*time.Second, 20*time.Millisecond,
		"an unadoptable backend with no referencer must be reaped on its first recovery pass")
	assert.Contains(t, logs.String(), "reaped unclaimed nbdkit process")
	_, ok := findMountedVolume(cfg, volumeName)
	assert.False(t, ok, "a backend recovery could not rebuild must never appear in MountedVolumes")

	// The kernel's real /proc would no longer list a PID that has exited;
	// mirror that so the second pass sees exactly what a real restart would.
	require.NoError(t, os.RemoveAll(filepath.Join(procRoot, strconv.Itoa(pid))))

	// Second restart's recovery pass (standing in for further stop/start
	// cycles): nothing survived, so there is nothing left to discover, reap
	// or log -- unlike the field case, which was still there after six.
	logs2 := captureLogs(t)
	recoverMountedVolumes(context.Background(), cfg, nc, procRoot)

	assert.NotContains(t, logs2.String(), "volume="+volumeName,
		"a reaped orphan must not be rediscovered or re-logged on a later restart")
	_, ok = findMountedVolume(cfg, volumeName)
	assert.False(t, ok)
}
