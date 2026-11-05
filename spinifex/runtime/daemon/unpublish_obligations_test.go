package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- nodeDeparted ---

func newDepartedDaemon(t *testing.T) (*Daemon, *failingKV) {
	t.Helper()
	m, f := newFaultInjectedJSM(t)
	return &Daemon{
		jsManager:     m,
		clusterConfig: &config.ClusterConfig{Epoch: 5, Nodes: map[string]config.Config{"node-a": {}, "node-b": {}}},
	}, f
}

func TestNodeDeparted_AMemberIsNeverDeparted(t *testing.T) {
	t.Parallel()
	d, f := newDepartedDaemon(t)
	f.setFail("Get", true)

	departed, err := d.nodeDeparted(t.Context(), "node-a")
	require.NoError(t, err)
	assert.False(t, departed, "a member that is down or unreachable keeps its obligations")
}

func TestNodeDeparted_ANonMemberDepartsOnlyWithNoCurrentHeartbeat(t *testing.T) {
	for name, tc := range map[string]struct {
		heartbeat *Heartbeat
		departed  bool
	}{
		"no heartbeat":                {departed: true},
		"heartbeat at an older epoch": {heartbeat: &Heartbeat{Node: "node-gone", Epoch: 4}, departed: true},
		"heartbeat at this epoch":     {heartbeat: &Heartbeat{Node: "node-gone", Epoch: 5}},
		"heartbeat at a later epoch":  {heartbeat: &Heartbeat{Node: "node-gone", Epoch: 6}},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newDepartedDaemon(t)
			if tc.heartbeat != nil {
				require.NoError(t, d.jsManager.WriteHeartbeat(tc.heartbeat))
			}

			departed, err := d.nodeDeparted(t.Context(), "node-gone")
			require.NoError(t, err)
			assert.Equal(t, tc.departed, departed)
		})
	}
}

func TestNodeDeparted_AnUnreadableHeartbeatIsAnError(t *testing.T) {
	t.Parallel()
	d, f := newDepartedDaemon(t)
	f.setFail("Get", true)

	departed, err := d.nodeDeparted(t.Context(), "node-gone")
	require.Error(t, err)
	assert.False(t, departed)
}

func TestNodeDeparted_NoClusterViewDepartsNothing(t *testing.T) {
	t.Parallel()
	departed, err := (&Daemon{}).nodeDeparted(t.Context(), "node-gone")
	require.NoError(t, err)
	assert.False(t, departed)
}

func TestClusterMembers(t *testing.T) {
	t.Parallel()
	d, _ := newDepartedDaemon(t)
	assert.Equal(t, []string{"node-a", "node-b"}, d.clusterMembers())
	assert.Nil(t, (&Daemon{}).clusterMembers())
}

// --- InstanceLastNode ---

func newLastNodeManager(t *testing.T) *JetStreamManager {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	m := &JetStreamManager{js: js}
	m.setInstanceStateBucket(kvstore.NewBucket(js, kvstore.Config{Name: "last-node-live", History: 1}))
	m.termRecords = kvstore.On[vm.InstanceRecord](kvstore.NewBucket(js, kvstore.Config{Name: "last-node-term", History: 1}))
	return m
}

func TestInstanceLastNode_ReadsTheLiveThenTheTerminatedRecord(t *testing.T) {
	t.Parallel()
	m := newLastNodeManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-live", (&vm.VM{ID: "i-live", LastNode: "node-a"}).Record()))
	require.NoError(t, m.WriteTerminatedInstanceRecord("i-term", (&vm.VM{ID: "i-term", LastNode: "node-b"}).Record()))

	node, found, err := m.InstanceLastNode("i-live")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "node-a", node)

	node, found, err = m.InstanceLastNode("i-term")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "node-b", node)

	_, found, err = m.InstanceLastNode("i-none")
	require.NoError(t, err)
	assert.False(t, found)
}

// A record that cannot be read is not a missing one: treating it as missing
// would name every member rather than fail the request.
func TestInstanceLastNode_AnUnreadableRecordIsAnError(t *testing.T) {
	t.Parallel()
	m, f := newFaultInjectedJSM(t)
	f.setFail("Get", true)

	_, found, err := m.InstanceLastNode("i-live")
	require.Error(t, err)
	assert.False(t, found)
}

// --- The mounter's volume hold ---

// holdTestProvider records each call and can hold a publish open until told.
type holdTestProvider struct {
	mu            sync.Mutex
	publishes     int
	unpublishes   []ebsprovider.UnpublishVolumeRequest
	unpublishErr  error
	publishGate   chan struct{}
	publishing    chan struct{}
	unpublishGate chan struct{}
	unpublishing  chan struct{}
}

var _ mounterProvider = (*holdTestProvider)(nil)

func (p *holdTestProvider) PublishVolume(_ context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	p.mu.Lock()
	p.publishes++
	p.mu.Unlock()
	if p.publishGate != nil {
		close(p.publishing)
		<-p.publishGate
	}
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, Export: ebsprovider.NewNBDExport("nbd://x")}, nil
}

func (p *holdTestProvider) UnpublishVolume(_ context.Context, req ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
	if p.unpublishGate != nil {
		close(p.unpublishing)
		<-p.unpublishGate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unpublishes = append(p.unpublishes, req)
	if p.unpublishErr != nil {
		return nil, p.unpublishErr
	}
	return &ebsprovider.UnpublishedVolume{TornDown: true}, nil
}

func (p *holdTestProvider) calls() []ebsprovider.UnpublishVolumeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ebsprovider.UnpublishVolumeRequest(nil), p.unpublishes...)
}

// A hot attach publishes before it writes in-use. The publish stamp is what
// lets the reaper tell that publish apart from the one an obligation undoes.
func TestVolumeMounterHold_PublishedSinceTracksThisProcessesPublishes(t *testing.T) {
	t.Parallel()
	provider := &holdTestProvider{}
	adapter := newVolumeMounterAdapter(provider, "node-hold")
	before := time.Now().Add(-time.Second)

	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-hold"}))

	hold, ok := adapter.HoldVolume("vol-hold")
	require.True(t, ok)
	assert.True(t, hold.PublishedSince(before), "a publish after the obligation's attachment is newer")
	assert.False(t, hold.PublishedSince(time.Now().Add(time.Second)), "a publish before it is not")
	hold.Release()

	require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-hold"}))
	hold, ok = adapter.HoldVolume("vol-hold")
	require.True(t, ok)
	defer hold.Release()
	assert.False(t, hold.PublishedSince(before), "an unpublish clears the stamp")
}

func TestVolumeMounterHold_RefusedWhileAPublishIsInFlight(t *testing.T) {
	t.Parallel()
	provider := &holdTestProvider{publishGate: make(chan struct{}), publishing: make(chan struct{})}
	adapter := newVolumeMounterAdapter(provider, "node-hold")
	done := make(chan error, 1)
	go func() {
		done <- adapter.MountOne(context.Background(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-busy"})
	}()
	<-provider.publishing

	_, ok := adapter.HoldVolume("vol-busy")
	assert.False(t, ok, "the reaper never waits behind a publish")

	close(provider.publishGate)
	require.NoError(t, <-done)
	hold, ok := adapter.HoldVolume("vol-busy")
	require.True(t, ok, "the lock is free once the publish returns")
	hold.Release()
	hold.Release()
	hold, ok = adapter.HoldVolume("vol-busy")
	require.True(t, ok, "a second release does not unlock someone else's hold")
	hold.Release()
}

// TestS8_AttachRefusesRatherThanWaitBehindTheReapersSeal enforces ADR-0003:S8:
// a publish arriving while the obligation reaper seals the volume answers at
// once, retryable, instead of waiting on a seal that scales with the volume.
func TestS8_AttachRefusesRatherThanWaitBehindTheReapersSeal(t *testing.T) {
	t.Parallel()
	provider := &holdTestProvider{unpublishGate: make(chan struct{}), unpublishing: make(chan struct{})}
	adapter := newVolumeMounterAdapter(provider, "node-hold")
	hold, ok := adapter.HoldVolume("vol-sealing")
	require.True(t, ok)
	sealed := make(chan error, 1)
	go func() { sealed <- hold.Unpublish(context.Background(), testAccountID, "handle-1", false) }()
	<-provider.unpublishing

	refused := make(chan error, 1)
	go func() {
		refused <- adapter.MountOne(context.Background(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-sealing"})
	}()
	select {
	case err := <-refused:
		require.ErrorIs(t, err, vm.ErrVolumeSettling)
		require.ErrorIs(t, err, vm.ErrMountRetryable)
	case <-time.After(5 * time.Second):
		t.Fatal("ADR-0003:S8: the attach waited behind the reaper's seal")
	}
	provider.mu.Lock()
	assert.Zero(t, provider.publishes, "a refused attach sends no publish")
	provider.mu.Unlock()

	close(provider.unpublishGate)
	require.NoError(t, <-sealed)
	hold.Release()
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-sealing"}),
		"the attach goes through once the seal is done")
}

// TestS8_AttachRefusesRatherThanWaitBehindAStopsSeal enforces ADR-0003:S8: a
// force detach clears the attachment while a stop still seals the volume, and
// an attach elsewhere on this node answers at once instead of waiting.
func TestS8_AttachRefusesRatherThanWaitBehindAStopsSeal(t *testing.T) {
	t.Parallel()
	provider := &holdTestProvider{unpublishGate: make(chan struct{}), unpublishing: make(chan struct{})}
	adapter := newVolumeMounterAdapter(provider, "node-hold")
	sealed := make(chan error, 1)
	go func() {
		sealed <- adapter.UnmountOne(context.Background(), testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-stopping"})
	}()
	<-provider.unpublishing

	refused := make(chan error, 1)
	go func() {
		refused <- adapter.MountOne(context.Background(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-stopping"})
	}()
	select {
	case err := <-refused:
		require.ErrorIs(t, err, vm.ErrVolumeSettling)
		require.ErrorIs(t, err, vm.ErrMountRetryable)
	case <-time.After(5 * time.Second):
		t.Fatal("ADR-0003:S8: the attach waited behind the stop's seal")
	}
	provider.mu.Lock()
	assert.Zero(t, provider.publishes, "a refused attach sends no publish")
	provider.mu.Unlock()

	close(provider.unpublishGate)
	require.NoError(t, <-sealed)
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-stopping"}),
		"the attach goes through once the seal is done")
}

// TestS8_AWaitingPublishGoesBeforeASealQueuedBehindIt enforces ADR-0003:S8: a
// publish waiting on a holder that does not seal is never overtaken by a seal
// that queued after it, so it never ends up waiting on that seal.
func TestS8_AWaitingPublishGoesBeforeASealQueuedBehindIt(t *testing.T) {
	t.Parallel()
	var locks volumeLocks
	first, ok := locks.acquire("vol-busy", acquirePublish)
	require.True(t, ok)

	waiters := func() int {
		locks.mu.Lock()
		defer locks.mu.Unlock()
		return locks.locks["vol-busy"].waiting
	}
	var (
		mu    sync.Mutex
		order []string
	)
	took := func(who string, entry *volumeLock, sealing bool) {
		mu.Lock()
		order = append(order, who)
		mu.Unlock()
		assert.Equal(t, sealing, entry.sealing, who)
		locks.release("vol-busy", entry)
	}
	done := make(chan struct{}, 2)
	go func() {
		entry, _ := locks.acquire("vol-busy", acquireSeal)
		took("seal", entry, true)
		done <- struct{}{}
	}()
	require.Eventually(t, func() bool { return waiters() == 1 }, 5*time.Second, time.Millisecond)
	go func() {
		entry, ok := locks.acquire("vol-busy", acquirePublish)
		assert.True(t, ok, "a publish behind a holder that does not seal waits rather than refuses")
		took("publish", entry, false)
		done <- struct{}{}
	}()
	require.Eventually(t, func() bool { return waiters() == 2 }, 5*time.Second, time.Millisecond)

	locks.release("vol-busy", first)
	<-done
	<-done
	assert.Equal(t, []string{"publish", "seal"}, order)
	locks.mu.Lock()
	assert.Empty(t, locks.locks, "an entry nobody holds or awaits is forgotten")
	locks.mu.Unlock()
}

func TestVolumeMounterHold_UnpublishSealsOrDropsAsAsked(t *testing.T) {
	t.Parallel()
	provider := &holdTestProvider{}
	adapter := newVolumeMounterAdapter(provider, "node-hold")

	hold, ok := adapter.HoldVolume("vol-settle")
	require.True(t, ok)
	require.NoError(t, hold.Unpublish(t.Context(), testAccountID, "handle-1", false))
	require.NoError(t, hold.Unpublish(t.Context(), testAccountID, "handle-1", true))
	hold.Release()

	calls := provider.calls()
	require.Len(t, calls, 2)
	for _, call := range calls {
		assert.Equal(t, "vol-settle", call.VolumeID)
		assert.Equal(t, "handle-1", call.Handle, "the obligation's handle names the publish it undoes")
		assert.Equal(t, "node-hold", call.NodeID)
		assert.Equal(t, ebsprovider.NoSealSchemaVersion, call.SchemaVersion)
	}
	assert.False(t, calls[0].NoSeal, "a surviving volume is sealed so its writes are kept")
	assert.True(t, calls[1].NoSeal)
}

func TestVolumeMounterHold_NotFoundCountsAsDone(t *testing.T) {
	t.Parallel()
	for _, noSeal := range []bool{false, true} {
		provider := &holdTestProvider{unpublishErr: ebsprovider.ErrNotFound}
		adapter := newVolumeMounterAdapter(provider, "node-hold")
		hold, ok := adapter.HoldVolume("vol-gone")
		require.True(t, ok)
		assert.NoError(t, hold.Unpublish(t.Context(), testAccountID, "", noSeal))
		hold.Release()
	}
}

// --- DeleteVolumes names this node for what the last unmount left ---

func TestInstanceCleaner_DeleteVolumes_RecordsOnlyTheOwedVolumes(t *testing.T) {
	t.Parallel()
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)
	obligations := wireTestUnpublishObligations(t, daemon)
	const instanceID = "i-owed-cleaner"
	for _, volumeID := range []string{"vol-owed-1", "vol-sealed-1"} {
		seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
			VolumeID: volumeID, TenantID: testAccountID, CapacityGiB: 1,
			State: "in-use", AttachedInstance: instanceID,
		})
	}
	instance := &vm.VM{ID: instanceID, AccountID: testAccountID, UnpublishOwed: []string{"vol-owed-1"}}
	instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-owed-1"}, {Name: "vol-sealed-1"}}

	require.NoError(t, newInstanceCleanerAdapter(daemon).DeleteVolumes(instance))

	open, err := obligations.List(t.Context())
	require.NoError(t, err)
	require.Len(t, open, 1, "a volume whose unmount completed owes nothing")
	assert.Equal(t, "vol-owed-1", open[0].VolumeID)
	assert.Equal(t, daemon.node, open[0].NodeID)
	for _, volumeID := range []string{"vol-owed-1", "vol-sealed-1"} {
		meta, err := daemon.volumeService.GetVolumeMetadata(testAccountID, volumeID)
		require.NoError(t, err)
		assert.Empty(t, meta.AttachedInstance)
	}
}
