package viperblock

//test:in-package — the fence, the lease store and the notice it sends are all
//unexported, and the ordering they guarantee has no exported surface.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangingHolderKV stands in for a lease store this node has just failed to
// reach: once armed, reading the holder blocks until released or cancelled.
type hangingHolderKV struct {
	jetstream.KeyValue

	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (k *hangingHolderKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if !k.armed.Load() {
		return k.KeyValue.Get(ctx, key)
	}
	select {
	case k.entered <- struct{}{}:
	default:
	}
	select {
	case <-k.release:
		return k.KeyValue.Get(ctx, key)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// hangingFenceConfig is fencedConfig with a lease store whose holder read hangs
// once armed. The wrapper goes in before the lease exists, so no renewal
// goroutine ever sees the store change under it.
func hangingFenceConfig(t *testing.T, natsURL, volumeName string) (*Config, *hangingHolderKV) {
	t.Helper()

	leases := newTestLeases(t, natsURL, "node-a")
	kv := &hangingHolderKV{KeyValue: leases.kv, entered: make(chan struct{}, 1), release: make(chan struct{})}
	leases.kv = kv

	cfg := &Config{NodeName: "node-a", leases: leases, dirty: newTestDirty(t, natsURL, "node-a")}
	cfg.leases = cfg.bindLeaseFence(cfg.leases)

	lease, err := cfg.acquireVolumeLease(t.Context(), volumeName)
	require.NoError(t, err)
	cfg.MountedVolumes = []MountedVolume{{Name: volumeName, Lease: lease, PID: stubExport(t)}}
	t.Cleanup(func() { cfg.releaseVolumeLease(context.Background(), lease) })

	kv.armed.Store(true)
	return cfg, kv
}

// TestS4_FenceStopsTheWriterBeforeReadingTheHolder enforces ADR-0007:S4: a
// fence must be "effective fencing that prevents the former node from continuing
// as a conflicting writer". Local validity lapses only volumeLeaseAcquireMargin
// before the server can re-grant the entry, so nbdkit has to be dead inside that
// margin even when the read naming the winner never comes back.
func TestS4_FenceStopsTheWriterBeforeReadingTheHolder(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencekillfirst"
	cfg, kv := hangingFenceConfig(t, natsURL, volumeName)
	pid := cfg.MountedVolumes[0].PID

	done := make(chan struct{})
	go func() { defer close(done); cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled) }()
	t.Cleanup(func() { close(kv.release); <-done })

	require.Eventuallyf(t, func() bool { return !hostprocess.ProcessAlive(pid) },
		volumeLeaseAcquireMargin, 10*time.Millisecond,
		"ADR-0007:S4: nbdkit (pid %d) was still alive %s after the lease was lost, with the holder read hanging; "+
			"the server re-grants the entry after that margin, so this node would still be writing when another takes over. "+
			"Fix: kill the export in fenceVolume (volume_fence.go) before anything that reads JetStream",
		pid, volumeLeaseAcquireMargin)

	select {
	case <-kv.entered:
	case <-time.After(volumeLeaseAcquireMargin):
		t.Fatal("the fence never read the holder after the kill")
	}
	cfg.mu.Lock()
	mounted := len(cfg.MountedVolumes)
	cfg.mu.Unlock()
	assert.Zero(t, mounted, "ADR-0007:S4: the export must be gone before the fence waits on the store")
}

// TestS4_FenceHolderReadIsBounded enforces ADR-0007:S4 for the part of the fence
// after the kill. The holder read only names the winner, and the daemon's notice
// waits behind it, so a store that never answers must not hold the guest up.
func TestS4_FenceHolderReadIsBounded(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencereadbounded"
	cfg, kv := hangingFenceConfig(t, natsURL, volumeName)
	t.Cleanup(func() { close(kv.release) })

	done := make(chan struct{})
	go func() { defer close(done); cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled) }()

	select {
	case <-kv.entered:
	case <-time.After(volumeLeaseAcquireMargin):
		t.Fatal("ADR-0007:S4: the fence never read the holder, so this test proved nothing about bounding it")
	}

	limit := fenceHolderReadTimeout + 2*time.Second
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("ADR-0007:S4: the fence was still waiting on an unanswered holder read after %s; "+
			"the read is diagnostics and must not delay the notice that stops the guest. "+
			"Fix: bound currentOwner's context in fenceWinner (volume_fence.go)", limit)
	}
}

// fenceResponder answers fence notices as the daemon does and records them.
type fenceResponder struct {
	mu     sync.Mutex
	events []viperblocklegacyv1.VolumeFencedEvent
}

func (r *fenceResponder) handle(msg *nats.Msg) {
	var event viperblocklegacyv1.VolumeFencedEvent
	_ = json.Unmarshal(msg.Data, &event)
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	payload, _ := json.Marshal(viperblocklegacyv1.VolumeFencedAck{Instance: "i-fenced"})
	_ = msg.Respond(payload)
}

func (r *fenceResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// noticeConfig is fencedConfig with a connection to announce on and a short
// retry interval.
func noticeConfig(t *testing.T, natsURL, volumeName string) (*Config, *nats.Conn) {
	t.Helper()
	cfg := fencedConfig(t, natsURL, "node-a", volumeName)
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	cfg.nc = nc
	cfg.fenceNoticeEvery = 20 * time.Millisecond
	return cfg, nc
}

// TestS4_FenceNoticeSurvivesAMissedDelivery enforces ADR-0007:S4 on the daemon's
// half of the fence. The notice is the only way the daemon learns its guest lost
// its disk, and a node's NATS restarting mid-fence leaves nobody subscribed for
// the first send. It has to arrive once the daemon is back, however late.
func TestS4_FenceNoticeSurvivesAMissedDelivery(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencenoticelate"
	cfg, _ := noticeConfig(t, natsURL, volumeName)

	done := make(chan struct{})
	go func() { defer close(done); cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled) }()

	// Nobody is listening yet: the daemon is still reconnecting.
	time.Sleep(10 * cfg.fenceNoticeEvery)

	daemonConn, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(daemonConn.Close)
	responder := &fenceResponder{}
	_, err = daemonConn.Subscribe(viperblocklegacyv1.VolumeFencedSubject("node-a"), responder.handle)
	require.NoError(t, err)
	require.NoError(t, daemonConn.Flush())

	select {
	case <-done:
	case <-time.After(fenceNoticeTimeout + 2*time.Second):
		t.Fatal("ADR-0007:S4: the fence never finished announcing to a daemon that came back")
	}
	require.GreaterOrEqualf(t, responder.count(), 1,
		"ADR-0007:S4: a daemon that subscribed after the first notice never heard the fence, so its guest runs on with no disk. "+
			"Fix: retry publishVolumeFenced (volume_fence.go) until the daemon acknowledges")
	assert.Equal(t, volumeName, responder.events[0].Volume)

	seen := responder.count()
	time.Sleep(5 * cfg.fenceNoticeEvery)
	assert.Equal(t, seen, responder.count(), "an acknowledged notice must not be sent again")
}

// TestS4_StaleFenceNoticeIsDropped is the limit on that retry. Once the volume is
// exported here again it belongs to a newer mount, and a notice that finally
// lands then would stop the guest that mount is serving.
func TestS4_StaleFenceNoticeIsDropped(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencenoticestale"
	cfg, _ := noticeConfig(t, natsURL, volumeName)

	done := make(chan struct{})
	go func() { defer close(done); cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled) }()

	require.Eventually(t, func() bool {
		cfg.mu.Lock()
		defer cfg.mu.Unlock()
		return len(cfg.MountedVolumes) == 0
	}, volumeLeaseAcquireMargin, 10*time.Millisecond)
	cfg.mu.Lock()
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName})
	cfg.mu.Unlock()

	select {
	case <-done:
	case <-time.After(fenceNoticeTimeout + 2*time.Second):
		t.Fatal("ADR-0007:S4: a fence notice kept retrying after its volume was exported here again")
	}
}

// TestS4_FenceNoticeStopsWithTheConnection keeps a shutting-down viperblockd from
// leaving a notice goroutine spinning on a connection that cannot carry it.
func TestS4_FenceNoticeStopsWithTheConnection(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-fencenoticeclosed"
	cfg, nc := noticeConfig(t, natsURL, volumeName)

	done := make(chan struct{})
	go func() { defer close(done); cfg.onVolumeLeaseLost(context.Background(), volumeName, leaseLostStalled) }()

	time.Sleep(5 * cfg.fenceNoticeEvery)
	nc.Close()

	select {
	case <-done:
	case <-time.After(fenceNoticeTimeout + 2*time.Second):
		t.Fatal("a fence notice kept retrying on a closed connection")
	}
}
