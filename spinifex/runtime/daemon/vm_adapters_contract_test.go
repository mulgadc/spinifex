package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var mounterTestNode atomic.Int64

// newMounterTestNode returns a node name no other test on the shared NATS
// server subscribes for, so per-node subjects never cross tests.
func newMounterTestNode() string {
	return fmt.Sprintf("node-mounter-%d", mounterTestNode.Add(1))
}

func connectMounterTest(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// replyJSON answers every request on subject with reply(request body).
func replyJSON(t *testing.T, nc *nats.Conn, subject string, reply func(data []byte) any) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		data, err := json.Marshal(reply(msg.Data))
		if err != nil {
			panic(err)
		}
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// advertiseVersions answers node's per-node capabilities subject with the
// versions versions() returns, and counts how often it was asked.
func advertiseVersions(t *testing.T, nc *nats.Conn, node string, versions func() []uint16) *atomic.Int32 {
	t.Helper()
	subject, err := ebsprovider.NodeCapabilitiesSubject(node)
	require.NoError(t, err)
	var asked atomic.Int32
	replyJSON(t, nc, subject, func([]byte) any {
		asked.Add(1)
		return ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), ContractVersions: versions()}
	})
	return &asked
}

// countLegacy answers node's legacy mount and unmount subjects successfully
// and counts the requests each received.
func countLegacy(t *testing.T, nc *nats.Conn, node string) (mounts, unmounts *atomic.Int32) {
	t.Helper()
	mounts, unmounts = new(atomic.Int32), new(atomic.Int32)
	replyJSON(t, nc, viperblocklegacyv1.MountSubject(node), func(data []byte) any {
		mounts.Add(1)
		var req viperblocklegacyv1.EBSRequest
		_ = json.Unmarshal(data, &req)
		return viperblocklegacyv1.EBSMountResponse{URI: "nbd://legacy-" + req.Name, Mounted: true}
	})
	replyJSON(t, nc, viperblocklegacyv1.UnmountSubject(node), func(data []byte) any {
		unmounts.Add(1)
		var req viperblocklegacyv1.EBSRequest
		_ = json.Unmarshal(data, &req)
		return viperblocklegacyv1.EBSUnMountResponse{Volume: req.Name}
	})
	return mounts, unmounts
}

// serveMemoryProvider serves a memory provider holding volumeIDs as node's
// local provider, listing every version this build serves.
func serveMemoryProvider(t *testing.T, nc *nats.Conn, node string, volumeIDs ...string) *ebsprovider.MemoryProvider {
	t.Helper()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	for _, id := range volumeIDs {
		_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
			Versioned:     ebsprovider.NewVersioned(),
			VolumeID:      id,
			CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 20},
		})
		require.NoError(t, err)
	}
	stop, err := natsserve.Serve(t.Context(), nc, provider, natsserve.Options{NoQueueGroup: true, NodeID: node})
	require.NoError(t, err)
	t.Cleanup(stop)
	return provider
}

func volumeState(t *testing.T, provider *ebsprovider.MemoryProvider, volumeID string) ebsprovider.VolumeState {
	t.Helper()
	volume, err := provider.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID})
	require.NoError(t, err)
	return volume.State
}

func mounterTestVM(volumeIDs ...string) *vm.VM {
	instance := &vm.VM{ID: "i-mounter", AccountID: testAccountID}
	for _, id := range volumeIDs {
		instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, viperblocklegacyv1.EBSRequest{Name: id})
	}
	return instance
}

// A local provider listing version 3 takes every mount and unmount, and the
// legacy subjects see none of them.
func TestVolumeMounterAdapter_ContractWhenLocalProviderListsVersion3(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	provider := serveMemoryProvider(t, nc, node, "vol-a", "vol-b", "vol-hot")
	legacyMounts, legacyUnmounts := countLegacy(t, nc, node)
	volState := &recordingVolState{}
	adapter := newVolumeMounterAdapter(nc, node, volState)

	instance := mounterTestVM("vol-a", "vol-b")
	require.NoError(t, adapter.Mount(t.Context(), instance))
	for _, req := range instance.EBSRequests.Requests {
		assert.Equal(t, "nbd+unix:///?socket=/memory/"+req.Name+".sock", req.NBDURI, "the published export must reach the request")
		assert.Equal(t, ebsprovider.VolumeStateInUse, volumeState(t, provider, req.Name))
	}

	hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot"}
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, hot))
	assert.Equal(t, "nbd+unix:///?socket=/memory/vol-hot.sock", hot.NBDURI)
	require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, *hot))
	assert.Equal(t, ebsprovider.VolumeStateAvailable, volumeState(t, provider, "vol-hot"))

	require.NoError(t, adapter.Unmount(t.Context(), instance))
	for _, id := range []string{"vol-a", "vol-b"} {
		assert.Equal(t, ebsprovider.VolumeStateAvailable, volumeState(t, provider, id), "unmount must unpublish %s", id)
	}
	assert.ElementsMatch(t, []string{"vol-a:available", "vol-b:available"}, volState.snapshot())

	assert.Zero(t, legacyMounts.Load(), "no mount may use the legacy subject")
	assert.Zero(t, legacyUnmounts.Load(), "no unmount may use the legacy subject")
}

// A provider that does not list version 3, or that predates the per-node
// capabilities subject, keeps the legacy subjects for every operation.
func TestVolumeMounterAdapter_LegacyWhenLocalProviderLacksVersion3(t *testing.T) {
	tests := []struct {
		name      string
		advertise bool
		versions  []uint16
	}{
		{name: "ListsVersions1And2", advertise: true, versions: []uint16{1, 2}},
		{name: "PredatesContractVersions", advertise: true, versions: nil},
		{name: "NoPerNodeCapabilitiesSubject", advertise: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			if tt.advertise {
				advertiseVersions(t, nc, node, func() []uint16 { return tt.versions })
			}
			var published atomic.Int32
			for _, build := range []func(string) (string, error){ebsprovider.PublishSubject, ebsprovider.UnpublishSubject} {
				subject, err := build(node)
				require.NoError(t, err)
				replyJSON(t, nc, subject, func([]byte) any {
					published.Add(1)
					return ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned()}
				})
			}
			legacyMounts, legacyUnmounts := countLegacy(t, nc, node)
			adapter := newVolumeMounterAdapter(nc, node, nil)

			instance := mounterTestVM("vol-a")
			require.NoError(t, adapter.Mount(t.Context(), instance))
			assert.Equal(t, "nbd://legacy-vol-a", instance.EBSRequests.Requests[0].NBDURI)
			hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot"}
			require.NoError(t, adapter.MountOne(t.Context(), testAccountID, hot))
			require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, *hot))
			require.NoError(t, adapter.Unmount(t.Context(), instance))

			assert.Equal(t, int32(2), legacyMounts.Load())
			assert.Equal(t, int32(2), legacyUnmounts.Load())
			assert.Zero(t, published.Load(), "no request may reach the contract subjects")
		})
	}
}

// fakeContractPublish answers node's publish subject with whatever the test
// has set, so a test can change the provider's answer between relaunches.
type fakeContractPublish struct {
	mu    sync.Mutex
	reply ebsprovider.PublishVolumeResponseV1
}

func (f *fakeContractPublish) set(reply ebsprovider.PublishVolumeResponseV1) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

func (f *fakeContractPublish) serve(t *testing.T, nc *nats.Conn, node string) {
	t.Helper()
	subject, err := ebsprovider.PublishSubject(node)
	require.NoError(t, err)
	replyJSON(t, nc, subject, func(data []byte) any {
		var req ebsprovider.PublishVolumeRequest
		_ = json.Unmarshal(data, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := f.reply
		reply.Versioned = ebsprovider.ReplyVersioned(req.SchemaVersion)
		if reply.Published != nil {
			published := *reply.Published
			published.VolumeID = req.VolumeID
			reply.Published = &published
		}
		return reply
	})
}

func publishRefusal(code ebsprovider.ErrorCode) ebsprovider.PublishVolumeResponseV1 {
	return ebsprovider.PublishVolumeResponseV1{Error: &ebsprovider.ProviderError{Code: code, Message: "refused: " + string(code)}}
}

// A relaunch refused as retry-later, or because the volume state is not found
// yet, backs off as restore expects and mounts once the provider is ready.
func TestVolumeMounterAdapter_RetryLaterRefusalBacksOffAndRecovers(t *testing.T) {
	for _, code := range []ebsprovider.ErrorCode{ebsprovider.ErrorCodeUnavailable, ebsprovider.ErrorCodeNotFound} {
		t.Run(string(code), func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			advertiseVersions(t, nc, node, ebsprovider.SupportedSchemaVersions)
			publish := &fakeContractPublish{}
			publish.set(publishRefusal(code))
			publish.serve(t, nc, node)
			adapter := newVolumeMounterAdapter(nc, node, nil)

			instance := mounterTestVM("vol-relaunch")
			err := adapter.Mount(t.Context(), instance)
			require.ErrorIs(t, err, vm.ErrMountRetryable, "restore must back off rather than give up")
			hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot"}
			require.ErrorIs(t, adapter.MountOne(t.Context(), testAccountID, hot), vm.ErrMountRetryable)

			publish.set(ebsprovider.PublishVolumeResponseV1{Published: &ebsprovider.PublishedVolumeV1{NodeID: node, NBDURI: "nbd://ready"}})
			require.NoError(t, adapter.Mount(t.Context(), instance))
			assert.Equal(t, "nbd://ready", instance.EBSRequests.Requests[0].NBDURI)
		})
	}
}

// A refusal that retrying cannot fix fails the mount outright, so restore
// does not back off forever against a provider that will never accept it.
func TestVolumeMounterAdapter_PermanentRefusalIsNotRetryable(t *testing.T) {
	for _, code := range []ebsprovider.ErrorCode{ebsprovider.ErrorCodeVolumeInUse, ebsprovider.ErrorCodeInternal, ebsprovider.ErrorCodeInvalidArgument} {
		t.Run(string(code), func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			advertiseVersions(t, nc, node, ebsprovider.SupportedSchemaVersions)
			publish := &fakeContractPublish{}
			publish.set(publishRefusal(code))
			publish.serve(t, nc, node)
			adapter := newVolumeMounterAdapter(nc, node, nil)

			err := adapter.Mount(t.Context(), mounterTestVM("vol-refused"))
			require.Error(t, err)
			assert.NotErrorIs(t, err, vm.ErrMountRetryable)
		})
	}
}

// A mount that finds no provider on the publish subject backs off, and the
// next one asks for capabilities again, so a provider restarted at another
// version is seen.
func TestVolumeMounterAdapter_NoResponderBacksOffAndRereadsCapabilities(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	var versionsMu sync.Mutex
	versions := ebsprovider.SupportedSchemaVersions()
	asked := advertiseVersions(t, nc, node, func() []uint16 {
		versionsMu.Lock()
		defer versionsMu.Unlock()
		return versions
	})
	legacyMounts, _ := countLegacy(t, nc, node)
	adapter := newVolumeMounterAdapter(nc, node, nil)

	instance := mounterTestVM("vol-starting")
	err := adapter.Mount(t.Context(), instance)
	require.ErrorIs(t, err, vm.ErrMountRetryable)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	assert.Equal(t, int32(1), asked.Load())

	// The provider comes back at a version without the contract mount path.
	versionsMu.Lock()
	versions = []uint16{1, 2}
	versionsMu.Unlock()

	require.NoError(t, adapter.Mount(t.Context(), instance))
	assert.Equal(t, int32(2), asked.Load(), "no responder must make the next mount re-read capabilities")
	assert.Equal(t, int32(1), legacyMounts.Load())
	assert.Equal(t, "nbd://legacy-vol-starting", instance.EBSRequests.Requests[0].NBDURI)

	require.NoError(t, adapter.Mount(t.Context(), instance))
	assert.Equal(t, int32(2), asked.Load(), "an answered route is cached until a request finds no responder")
}

// An unmount that finds no provider fails the seal, so the volume does not go
// available, and the route is re-read on the next call.
func TestVolumeMounterAdapter_UnmountNoResponderKeepsVolumeAndRereads(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	asked := advertiseVersions(t, nc, node, ebsprovider.SupportedSchemaVersions)
	volState := &recordingVolState{}
	adapter := newVolumeMounterAdapter(nc, node, volState)

	instance := mounterTestVM("vol-gone")
	err := adapter.Unmount(t.Context(), instance)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	assert.Empty(t, volState.snapshot(), "an unsealed volume must not go available")

	require.Error(t, adapter.UnmountOne(t.Context(), testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-gone"}))
	assert.Equal(t, int32(2), asked.Load())
}

// The unpublish budget is the provider's data-movement budget, not the
// request timeout every other call gets: a seal that outlasts the request
// timeout still completes, where a publish that slow fails.
func TestVolumeMounterAdapter_UnpublishOutlastingRequestTimeoutCompletes(t *testing.T) {
	const requestTimeout = 100 * time.Millisecond
	const sealTime = 4 * requestTimeout

	nc := connectMounterTest(t)
	node := newMounterTestNode()
	advertiseVersions(t, nc, node, ebsprovider.SupportedSchemaVersions)
	slow := func([]byte) any {
		time.Sleep(sealTime)
		return ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned()}
	}
	unpublishSubject, err := ebsprovider.UnpublishSubject(node)
	require.NoError(t, err)
	replyJSON(t, nc, unpublishSubject, slow)
	publishSubject, err := ebsprovider.PublishSubject(node)
	require.NoError(t, err)
	replyJSON(t, nc, publishSubject, slow)

	volState := &recordingVolState{}
	adapter := newVolumeMounterAdapter(nc, node, volState)
	adapter.provider = ebsprovider.NewNATSProvider(nc, requestTimeout)

	require.NoError(t, adapter.Unmount(t.Context(), mounterTestVM("vol-slow-seal")))
	assert.Equal(t, []string{"vol-slow-seal:available"}, volState.snapshot())
	require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-slow-seal"}))

	err = adapter.MountOne(t.Context(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-slow-seal"})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the publish must be held to the request timeout")
}

// A failed volume rolls back the volumes this Mount already published, and a
// missing volume backs off rather than failing the relaunch.
func TestVolumeMounterAdapter_ContractMountRollsBackOnFailure(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	provider := serveMemoryProvider(t, nc, node, "vol-present")
	adapter := newVolumeMounterAdapter(nc, node, nil)

	err := adapter.Mount(t.Context(), mounterTestVM("vol-present", "vol-missing"))
	require.ErrorIs(t, err, vm.ErrMountRetryable)
	assert.ErrorIs(t, err, ebsprovider.ErrNotFound)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, volumeState(t, provider, "vol-present"), "the published volume must be unpublished")
}

// stubMounterProvider publishes a fixed export, for exports no NATS reply at
// the mounter's request version can carry.
type stubMounterProvider struct {
	export ebsprovider.VolumeExport
}

var _ mounterProvider = (*stubMounterProvider)(nil)

func (s *stubMounterProvider) PublishVolume(_ context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, Export: s.export}, nil
}

func (s *stubMounterProvider) UnpublishVolume(context.Context, ebsprovider.UnpublishVolumeRequest) (*ebsprovider.UnpublishedVolume, error) {
	return &ebsprovider.UnpublishedVolume{}, nil
}

func (s *stubMounterProvider) GetNodeCapabilities(context.Context, string, ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	return &ebsprovider.GetCapabilitiesResponse{ContractVersions: ebsprovider.SupportedSchemaVersions()}, nil
}

// A block export replaces whatever export the request carried before, so a
// relaunch never leaves both an NBD URI and a block device set.
func TestVolumeMounterAdapter_BlockExportReplacesNBDURI(t *testing.T) {
	adapter := newVolumeMounterAdapter(nil, "node-stub", nil)
	adapter.provider = &stubMounterProvider{export: ebsprovider.VolumeExport{
		Kind:  ebsprovider.ExportKindBlock,
		Block: &ebsprovider.BlockExport{DevicePath: "/dev/sdx", SizeBytes: 1 << 30},
	}}

	req := &viperblocklegacyv1.EBSRequest{Name: "vol-block", NBDURI: "nbd://stale"}
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, req))
	assert.Empty(t, req.NBDURI)
	assert.Equal(t, "/dev/sdx", req.BlockDevice)
	assert.Equal(t, uint64(1<<30), req.BlockSizeBytes)

	adapter.provider = &stubMounterProvider{export: ebsprovider.NewNBDExport("nbd://fresh")}
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, req))
	assert.Equal(t, "nbd://fresh", req.NBDURI)
	assert.Empty(t, req.BlockDevice)
	assert.Zero(t, req.BlockSizeBytes)
}
