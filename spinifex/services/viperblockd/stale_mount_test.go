package viperblockd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	releaseStaleMount(t.Context(), cfg, "vol-1")
	require.Len(t, cfg.MountedVolumes, 1)
	assert.Equal(t, "vol-2", cfg.MountedVolumes[0].Name)

	releaseStaleMount(t.Context(), cfg, "vol-absent")
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
