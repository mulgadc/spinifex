package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fenceActionNotice(volumeID string, action ebsprovider.FenceAction, generation uint64) ebsprovider.FenceNotice {
	return ebsprovider.FenceNotice{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-a",
		Action: action, Generation: generation,
	}
}

// A pause for a guest whose process cannot be pinned is answered by stopping
// it, as a fence always has, and acknowledged naming it.
func TestFencePause_AnUnpinnableGuestIsStoppedInstead(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-pauseunpinned")

	ack := requestFenceNotice(t, nc, fenceNoticePayload(t, fenceActionNotice("vol-pauseunpinned", ebsprovider.FenceActionPause, 7)))
	require.Nil(t, ack.Error)
	assert.Equal(t, guest.ID, ack.InstanceID)
	assert.Equal(t, vm.StateError, d.vmMgr.Status(guest), "a guest that cannot be paused safely is stopped")
}

// A resume for a guest not paused under that fence is refused, so the provider
// gives its export back rather than leave a guest it never resumed.
func TestFenceResume_IsRefusedForAGuestThatIsNotPaused(t *testing.T) {
	_, _, nc := fenceDaemon(t, "vol-resumenotpaused")

	ack := requestFenceNotice(t, nc, fenceNoticePayload(t, fenceActionNotice("vol-resumenotpaused", ebsprovider.FenceActionResume, 7)))
	require.NotNil(t, ack.Error, "a resume with nothing paused must be refused")
}

// A stop that names a fence generation acts only on a guest paused under it,
// so a stale stop leaves the running guest alone.
func TestFenceStop_WithAGenerationLeavesAGuestNotPausedUnderIt(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-stalestop")

	ack := requestFenceNotice(t, nc, fenceNoticePayload(t, fenceActionNotice("vol-stalestop", ebsprovider.FenceActionStop, 7)))
	require.Nil(t, ack.Error)
	assert.Empty(t, ack.InstanceID, "a stale stop stops nothing")
	assert.Equal(t, vm.StateRunning, d.vmMgr.Status(guest))
}

// A notice with an action this daemon does not know is refused, never guessed.
func TestFenceNotice_AnUnknownActionIsRefused(t *testing.T) {
	_, _, nc := fenceDaemon(t, "vol-unknownaction")

	ack := requestFenceNotice(t, nc, fenceNoticePayload(t, fenceActionNotice("vol-unknownaction", "teleport", 7)))
	require.NotNil(t, ack.Error)
}
