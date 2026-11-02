package viperblock

//test:in-package — mountEntryIsStale and releaseStaleMount are unexported, and
//exporting them would widen the package API for a test rather than for a caller.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A mount entry pointing at a socket that no longer exists describes an export
// that is gone. Returning its URI is what makes a volume permanently
// unstartable: every caller then waits for a socket nothing will create.
func TestMountEntryIsStaleWhenSocketIsGone(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nbd.sock")

	stale, why := mountEntryIsStale(MountedVolume{Name: "vol-1", Socket: socket, PID: os.Getpid()})
	assert.True(t, stale, "an absent socket means the export is gone")
	assert.Equal(t, "socket is gone", why)

	require.NoError(t, os.WriteFile(socket, nil, 0o600))
	stale, _ = mountEntryIsStale(MountedVolume{Name: "vol-1", Socket: socket, PID: os.Getpid()})
	assert.False(t, stale, "a live export must never be treated as stale: that is the double-writer hazard")
}

// The pid is the second piece of positive evidence, for an export whose socket
// file outlived the process that owned it.
func TestMountEntryIsStaleWhenProcessIsGone(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nbd.sock")
	require.NoError(t, os.WriteFile(socket, nil, 0o600))

	// PID 0 is "unknown", not dead, so it must not be read as evidence.
	stale, _ := mountEntryIsStale(MountedVolume{Name: "vol-1", Socket: socket, PID: 0})
	assert.False(t, stale, "an unknown pid is not evidence of death")

	stale, why := mountEntryIsStale(MountedVolume{Name: "vol-1", Socket: socket, PID: deadPID(t)})
	assert.True(t, stale, "a dead nbdkit means the export is gone")
	assert.Equal(t, "nbdkit process is gone", why)
}

// releaseStaleMount must drop the entry so the remount behind it can proceed,
// and must be safe to call for a volume that is not mounted at all.
func TestReleaseStaleMountDropsTheEntry(t *testing.T) {
	cfg := &Config{MountedVolumes: []MountedVolume{{Name: "vol-1"}, {Name: "vol-2"}}}

	require.NoError(t, releaseStaleMount(t.Context(), cfg, "vol-1"))
	require.Len(t, cfg.MountedVolumes, 1)
	assert.Equal(t, "vol-2", cfg.MountedVolumes[0].Name)

	require.NoError(t, releaseStaleMount(t.Context(), cfg, "vol-absent"))
	assert.Len(t, cfg.MountedVolumes, 1, "releasing an unmounted volume must be a no-op")
}

// deadPID returns a pid that has certainly exited.
func deadPID(t *testing.T) int {
	t.Helper()
	proc, err := os.StartProcess("/bin/true", []string{"/bin/true"}, &os.ProcAttr{})
	require.NoError(t, err)
	state, err := proc.Wait()
	require.NoError(t, err)
	return state.Pid()
}

// orphanedConfig is fencedConfig with the entry's socket gone and local state
// under BaseDir, so a stop through the unmount path has a copy to seal.
func orphanedConfig(t *testing.T, volumeName string, pid int) *Config {
	t.Helper()
	_, natsURL := setupEmbeddedNATS(t)
	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	withLocalState(t, cfg, volumeName)
	cfg.MountedVolumes[0].Socket = filepath.Join(t.TempDir(), "gone.sock")
	if pid != 0 {
		cfg.MountedVolumes[0].PID = pid
	}
	return cfg
}

// An nbdkit whose socket is gone still serves a client connected before it
// went, so a remount beside it is a second writer. It has to be stopped, and
// its copy sealed as any unmount would, before the volume is opened again.
func TestMountVolume_SocketGoneProcessAliveIsStoppedBeforeRemount(t *testing.T) {
	const volumeName = "vol-orphanalive1"
	cfg := orphanedConfig(t, volumeName, 0)
	pid := cfg.MountedVolumes[0].PID
	require.True(t, hostprocess.ProcessAlive(pid))

	sealed := false
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }
	errNoRemount := errors.New("remount stops here")
	var aliveAtRemount []bool
	cfg.constructVB = func(context.Context, string) (*viperblock.VB, int, error) {
		aliveAtRemount = append(aliveAtRemount, hostprocess.ProcessAlive(pid))
		return nil, 0, errNoRemount
	}

	_, err := mountVolume(t.Context(), cfg, nil, volumeName, false)
	require.ErrorIs(t, err, errNoRemount, "the remount must be reached once the old nbdkit is stopped")
	assert.Equal(t, []bool{false}, aliveAtRemount, "the old nbdkit must be gone before the volume is opened again")
	assert.True(t, sealed, "the old export's copy is sealed as a normal unmount would seal it")
}

// An nbdkit that will not stop is still a writer, so the publish is refused as
// retry-later and the entry kept for the next attempt to retry the stop.
func TestMountVolume_SocketGoneProcessUnstoppableIsUnavailable(t *testing.T) {
	const volumeName = "vol-orphanstuck1"
	cfg := orphanedConfig(t, volumeName, zombiePID(t))
	cfg.exportStopWait = 50 * time.Millisecond
	sealed := false
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }
	cfg.constructVB = func(context.Context, string) (*viperblock.VB, int, error) {
		t.Error("a volume must not be opened again beside an nbdkit that would not stop")
		return nil, 0, errors.New("unreachable")
	}

	_, err := mountVolume(t.Context(), cfg, nil, volumeName, false)
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable)
	assert.Equal(t, ebsprovider.ErrorCodeUnavailable, publishError(ebsprovider.SchemaVersion, volumeName, err).Code)
	_, mounted := findMountedVolume(cfg, volumeName)
	assert.True(t, mounted, "the entry must stay while its nbdkit may still write")
	assert.False(t, sealed, "nothing may seal underneath a writer that is still running")
}

// zombiePID returns an exited, unreaped child: kill(pid,0) still reaches it and
// no signal makes it go, so it stands in for an nbdkit that will not stop.
func zombiePID(t *testing.T) int {
	t.Helper()
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Process.Kill())
	var info unix.Siginfo
	require.NoError(t, unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil))
	t.Cleanup(func() { _ = cmd.Wait() })
	return cmd.Process.Pid
}
