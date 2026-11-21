package viperblock

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// pausedFenceConfig is a socket export whose dirty marker names this node at
// the lease's generation, with a daemon that records every notice, and a pause
// window short enough to watch end.
func pausedFenceConfig(t *testing.T, natsURL, volumeName string) (*Config, *fenceResponder) {
	t.Helper()
	cfg, _ := noticeConfig(t, natsURL, volumeName)
	cfg.MountedVolumes[0].Socket = filepath.Join(t.TempDir(), "nbd.sock")
	require.NoError(t, cfg.dirty.mark(t.Context(), volumeName, cfg.MountedVolumes[0].Lease.generation, "test"))
	cfg.fencePauseFor = 400 * time.Millisecond
	cfg.fenceRetakeEvery = 40 * time.Millisecond

	daemonConn, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(daemonConn.Close)
	responder := &fenceResponder{}
	subject, err := ebsprovider.FenceNoticeSubject("node-a")
	require.NoError(t, err)
	_, err = daemonConn.Subscribe(subject, responder.handle)
	require.NoError(t, err)
	require.NoError(t, daemonConn.Flush())
	return cfg, responder
}

func (r *fenceResponder) actions() []ebsprovider.FenceAction {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ebsprovider.FenceAction, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Action)
	}
	return out
}

// A stall with no peer pauses the guest, and a node that cannot take the
// volume back before the window ends stops it, as a fence always has.
func TestAStalledFencePausesThenStopsWhenTheWindowEnds(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	const volumeName = "vol-fencepausewindow"
	cfg, responder := pausedFenceConfig(t, natsURL, volumeName)

	cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled)

	require.Eventually(t, func() bool { return len(responder.actions()) >= 2 }, 5*time.Second, 20*time.Millisecond,
		"a paused guest whose node never took the volume back was never stopped")
	actions := responder.actions()
	require.Equal(t, ebsprovider.FenceActionPause, actions[0], "a stall with no peer pauses the guest first")
	require.Equal(t, ebsprovider.FenceActionStop, actions[len(actions)-1], "the window ending stops the guest")
}

// A dirty marker that names another node means that node has mounted the
// volume since, so the guest is stopped at once rather than resumed.
func TestAStalledFenceStopsWhenTheMarkerMoves(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	const volumeName = "vol-fencepausemoved"
	cfg, responder := pausedFenceConfig(t, natsURL, volumeName)
	cfg.fencePauseFor = time.Minute
	require.NoError(t, newTestDirty(t, natsURL, "node-b").mark(t.Context(), volumeName, 99, "took it"))

	cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled)

	require.Eventually(t, func() bool { return len(responder.actions()) >= 2 }, 5*time.Second, 20*time.Millisecond,
		"a guest whose volume another node has mounted was left paused")
	actions := responder.actions()
	require.Equal(t, ebsprovider.FenceActionPause, actions[0])
	require.Equal(t, ebsprovider.FenceActionStop, actions[1], "a moved marker stops the guest well before the window")
	require.Equal(t, "node-b", responder.events[1].Winner, "the stop names the node the marker names")
}

// A retake's cleanup withdraws only the export it created. An export of the
// volume started by a newer mount is left alone.
func TestARetakeNeverWithdrawsAnotherMountsExport(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	const volumeName = "vol-retakeother"
	cfg, _ := noticeConfig(t, natsURL, volumeName)
	newer := cfg.MountedVolumes[0]
	ours := newer
	ours.PID = newer.PID + 1

	require.True(t, cfg.abandonExport(t.Context(), ours, "resume not completed"))
	_, still := cfg.lookupMountedVolume(volumeName)
	require.True(t, still, "the retake withdrew an export it did not create")
}

// A retake reopens the volume without rewriting its marker, so a retake that
// fails before its export is up leaves the fenced marker for the next attempt.
func TestARetakeLeavesTheFencedMarkerUntilItsExportIsUp(t *testing.T) {
	t.Parallel()

	_, natsURL := setupEmbeddedNATS(t)
	const volumeName = "vol-retakemarker"
	cfg, _ := noticeConfig(t, natsURL, volumeName)
	fenced := cfg.MountedVolumes[0].Lease.generation
	require.NoError(t, cfg.dirty.mark(t.Context(), volumeName, fenced, "test"))
	cfg.releaseVolumeLease(t.Context(), cfg.MountedVolumes[0].Lease)
	cfg.MountedVolumes = nil

	lease, err := cfg.acquireVolumeLease(withFencedGeneration(t.Context(), fenced), volumeName)
	require.NoError(t, err)
	t.Cleanup(func() { cfg.releaseVolumeLease(context.Background(), lease) })

	record, ok, err := cfg.dirty.lookup(t.Context(), volumeName)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fenced, record.Generation, "a retake must not move the marker before its export is up")
}
