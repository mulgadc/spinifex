package vm

import (
	"testing"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
)

// TestStalledFencePausesTheGuest reproduces the 2026-11-20 outcome. A lease that
// lapsed with no peer holding the volume fences the export, and the guest is
// paused rather than killed, so it can resume if its node takes the lease back.
func TestStalledFencePausesTheGuest(t *testing.T) {
	m, _, pid, qmpCalls, _ := fencedGuest(t, "vol-stalledfence")

	m.FenceVolume(t.Context(), "vol-stalledfence", "volume lease could not be confirmed and no peer holds it")
	m.goroutineWg.Wait()

	assert.Truef(t, hostprocess.ProcessAlive(pid),
		"the guest (pid %d) was killed although no other node holds its volume", pid)
	assert.Equal(t, 1, qmpCalls("stop"), "the guest must be paused while its node tries to take the lease back")
}
