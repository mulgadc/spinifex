package viperblockd

//test:in-package — the fence, the lease store and MountedVolumes are all
//unexported, and the decision the fence makes has no exported surface.

import (
	"context"
	"os/exec"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubExport starts a real process to stand in for nbdkit. The fence must
// observe the writer gone before it does anything else, so a fixture with a
// fake PID would exercise the failure path and prove nothing about the kill.
//
// Reaped on its own goroutine as the mount path does, because kill(pid,0)
// succeeds against a zombie: an unreaped child never looks like it exited.
func stubExport(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd.Process.Pid
}

// fencedConfig builds a Config owning volumeName with one mounted entry, as a
// node that has an export up would have.
func fencedConfig(t *testing.T, natsURL, owner, volumeName string) *Config {
	t.Helper()

	cfg := &Config{
		NodeName: owner,
		leases:   newTestLeases(t, natsURL, owner),
		dirty:    newTestDirty(t, natsURL, owner),
	}
	cfg.leases = cfg.bindLeaseFence(cfg.leases)

	lease, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)
	cfg.MountedVolumes = []MountedVolume{{Name: volumeName, Lease: lease, PID: stubExport(t)}}

	// Stop the renewal goroutine before the embedded server goes away, or it
	// spends the TTL surrendering a lease no test is looking at any more.
	t.Cleanup(func() { cfg.releaseVolumeLease(context.Background(), lease) })
	return cfg
}

// TestVolumeFence_TearsDownWhenAnotherNodeHoldsTheLease is the property the
// fence exists for. A node that has lost its lease is a second writer, and the
// only way to stop being one without a backend that can refuse is to give up
// the export.
func TestVolumeFence_TearsDownWhenAnotherNodeHoldsTheLease(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencelost"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	pid := cfg.MountedVolumes[0].PID

	// node-b takes the volume, which is what the renewal would discover.
	winner := newTestLeases(t, natsURL, "node-b")
	require.NoError(t, winner.kv.Delete(t.Context(), volumeName))
	_, err := winner.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	cfg.onVolumeLeaseLost(t.Context(), volumeName, leaseLostToPeer)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	assert.Zero(t, mounted, "a fenced volume must not be left exported: the export is the second writer")
	assert.False(t, utils.ProcessAlive(pid), "the export process has to be gone, not merely forgotten")
}

// TestVolumeFence_FencesAnEntryNobodyHolds covers the lapsed case. An entry that
// aged out with no successor is not proof this node is safe to keep writing —
// it is proof this node cannot tell. Stopping one guest is the cheaper error
// than leaving two nodes writing one volume.
func TestVolumeFence_FencesAnEntryNobodyHolds(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencelapsed"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	require.NoError(t, cfg.leases.kv.Delete(t.Context(), volumeName))

	cfg.onVolumeLeaseLost(t.Context(), volumeName, leaseLostToPeer)

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	assert.Zero(t, mounted, "a lease this node cannot prove it holds must not stay exported")
}

// TestVolumeFence_KillFailureLeavesTheVolumeMounted pins the ordering the fence
// depends on. Releasing the lease or announcing the fence after a kill that did
// not take would tell the cluster a writer stopped that is still running, which
// is worse than not fencing at all.
func TestVolumeFence_KillFailureLeavesTheVolumeMounted(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencekillfails"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	cfg.MountedVolumes[0].PID = 0 // rejected by ForceKillProcess, so the kill cannot succeed

	cfg.fenceVolume(t.Context(), volumeName, "node-b", "taken")

	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	require.Equal(t, 1, mounted, "a fence that could not stop the writer must not report the volume released")

	owner, held := cfg.leases.currentOwner(t.Context(), volumeName)
	require.True(t, held, "the lease must not be released while this node is still exporting")
	assert.Equal(t, "node-a", owner)
}

// TestVolumeFence_DoesNotSealOnTheWayOut pins the one thing the fence must
// never do. This node's copy is the stale one, so sealing it would publish an
// older state over the winner's — the corruption the fence exists to prevent.
func TestVolumeFence_DoesNotSealOnTheWayOut(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencenoseal"

	sealed := false
	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }

	cfg.fenceVolume(t.Context(), volumeName, "node-b", "taken")

	assert.False(t, sealed, "fencing must never seal: this node's copy is behind the winner's")
}

// TestVolumeFence_LeavesTheDirtyMarkerToTheWinner covers the record an operator
// reads afterwards. The winner takes the marker over when it opens the volume,
// so a fencing node clearing it would erase the only note that writes were lost.
func TestVolumeFence_LeavesTheDirtyMarkerToTheWinner(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencekeepsmarker"

	cfg := fencedConfig(t, natsURL, "node-a", volumeName)

	cfg.fenceVolume(t.Context(), volumeName, "node-b", "taken")

	record, ok := cfg.dirty.holder(t.Context(), volumeName)
	require.True(t, ok, "fencing must leave the marker behind for the winner to take over")
	assert.Equal(t, "node-a", record.Owner)
}

// TestVolumeFence_AlreadyUnmountedIsNotAnError covers the ordinary race, where
// a volume is unmounted normally between losing its lease and the fence running.
func TestVolumeFence_AlreadyUnmountedIsNotAnError(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfg := &Config{
		NodeName: "node-a",
		leases:   newTestLeases(t, natsURL, "node-a"),
		dirty:    newTestDirty(t, natsURL, "node-a"),
	}

	cfg.fenceVolume(t.Context(), "vol-fencenotmounted", "node-b", "taken")
}

// TestVolumeFencedSubject_IsNodeAddressed pins the routing. A fenced guest is on
// the node that lost the volume, so a subject without the node in it would reach
// daemons with nothing to stop and, on a queue group, miss the one that has.
func TestVolumeFencedSubject_IsNodeAddressed(t *testing.T) {
	assert.Equal(t, "ebs.bottlebrush.fenced", VolumeFencedSubject("bottlebrush"))
	assert.Equal(t, "ebs.fenced", VolumeFencedSubject(""),
		"a single-node daemon has no node name, and still has to hear its own fences")
}
