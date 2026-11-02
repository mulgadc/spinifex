//go:build e2e

package harness

import (
	"testing"
	"time"
)

// A suite already marked broken must skip the caller rather than answer false
// at once: callers read false as "waited the whole budget and the guest never
// answered", and report a timeout for a wait that never happened.
func TestTryReadySkipsOnceTheSuiteIsBroken(t *testing.T) {
	var h SSHHealth
	h.broken.Store(true)

	var probe *testing.T
	returned := false
	t.Run("probe", func(t *testing.T) {
		probe = t
		h.TryReady(t, "192.0.2.1", 22, "/nonexistent", time.Millisecond)
		returned = true
	})

	if !probe.Skipped() {
		t.Fatal("TryReady on a broken suite did not skip its caller")
	}
	if returned {
		t.Fatal("TryReady on a broken suite returned to its caller instead of skipping")
	}
}
