package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var mounterTestNode atomic.Int64

// newMounterTestNode returns a node name no other test on the shared NATS
// server subscribes for, so per-node subjects never cross tests.
func newMounterTestNode() string {
	return fmt.Sprintf("node-mounter-%d", mounterTestNode.Add(1))
}

// newNATSMounter returns a mounter driving node's provider over nc.
func newNATSMounter(nc *nats.Conn, node string, volState vm.VolumeStateUpdater) *volumeMounterAdapter {
	return newVolumeMounterAdapter(ebsprovider.NewNATSProvider(nc, ebsProviderRequestTimeout), node, volState)
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

// newMemoryProvider returns a memory provider holding volumeIDs.
func newMemoryProvider(t *testing.T, volumeIDs ...string) *ebsprovider.MemoryProvider {
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
	return provider
}

// serveMemoryProvider serves provider as nodeID's local provider.
func serveMemoryProvider(t *testing.T, nc *nats.Conn, provider *ebsprovider.MemoryProvider, nodeID string) {
	t.Helper()
	stop, err := natsserve.Serve(t.Context(), nc, provider, natsserve.Options{NoQueueGroup: true, NodeID: nodeID})
	require.NoError(t, err)
	t.Cleanup(stop)
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

// contractCall is one request a fakeNodeProvider or a spy received.
type contractCall struct {
	subject string
	version uint16
	volume  string
	noSeal  bool
	account string
}

// fakeNodeProvider answers node's publish and unpublish subjects with the
// replies the test sets, echoing each request's version unless a publish reply
// sets its own, and records them.
type fakeNodeProvider struct {
	mu        sync.Mutex
	publish   func(volumeID string) ebsprovider.PublishVolumeResponse
	unpublish func(volumeID string) ebsprovider.UnpublishVolumeResponse
	calls     []contractCall
}

// serveFakeNodeProvider starts a fake that publishes every volume as an NBD
// export and unpublishes it successfully until the test says otherwise.
func serveFakeNodeProvider(t *testing.T, nc *nats.Conn, node string) *fakeNodeProvider {
	t.Helper()
	f := &fakeNodeProvider{
		publish:   func(volumeID string) ebsprovider.PublishVolumeResponse { return publishedNBD("nbd://" + volumeID) },
		unpublish: func(string) ebsprovider.UnpublishVolumeResponse { return ebsprovider.UnpublishVolumeResponse{} },
	}
	publishSubject, err := ebsprovider.PublishSubject(node)
	require.NoError(t, err)
	replyJSON(t, nc, publishSubject, func(data []byte) any {
		var req ebsprovider.PublishVolumeRequest
		_ = json.Unmarshal(data, &req)
		f.mu.Lock()
		f.calls = append(f.calls, contractCall{subject: publishSubject, version: req.SchemaVersion, volume: req.VolumeID})
		reply := f.publish
		f.mu.Unlock()
		resp := reply(req.VolumeID)
		if resp.SchemaVersion == 0 {
			resp.Versioned = ebsprovider.ReplyVersioned(req.SchemaVersion)
		}
		if resp.Published != nil {
			published := *resp.Published
			published.VolumeID, published.NodeID = req.VolumeID, req.NodeID
			resp.Published = &published
		}
		return resp
	})
	unpublishSubject, err := ebsprovider.UnpublishSubject(node)
	require.NoError(t, err)
	replyJSON(t, nc, unpublishSubject, func(data []byte) any {
		var req ebsprovider.UnpublishVolumeRequest
		_ = json.Unmarshal(data, &req)
		f.mu.Lock()
		f.calls = append(f.calls, contractCall{subject: unpublishSubject, version: req.SchemaVersion, volume: req.VolumeID, noSeal: req.NoSeal})
		reply := f.unpublish
		f.mu.Unlock()
		resp := reply(req.VolumeID)
		resp.Versioned = ebsprovider.ReplyVersioned(req.SchemaVersion)
		return resp
	})
	return f
}

func (f *fakeNodeProvider) setPublish(reply func(volumeID string) ebsprovider.PublishVolumeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publish = reply
}

func (f *fakeNodeProvider) setUnpublish(reply func(volumeID string) ebsprovider.UnpublishVolumeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpublish = reply
}

func (f *fakeNodeProvider) snapshot() []contractCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contractCall(nil), f.calls...)
}

// unpublished returns, in order, the volumes the fake was asked to unpublish.
func (f *fakeNodeProvider) unpublished() []string {
	var volumes []string
	for _, call := range f.snapshot() {
		if strings.HasSuffix(call.subject, ".unmount") {
			volumes = append(volumes, call.volume)
		}
	}
	return volumes
}

func publishedNBD(uri string) ebsprovider.PublishVolumeResponse {
	return ebsprovider.PublishVolumeResponse{Published: &ebsprovider.PublishedVolume{Export: ebsprovider.NewNBDExport(uri)}}
}

func providerRefusal(code ebsprovider.ErrorCode, message string) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: code, Message: message}
}

func publishRefusal(code ebsprovider.ErrorCode) func(string) ebsprovider.PublishVolumeResponse {
	return func(string) ebsprovider.PublishVolumeResponse {
		return ebsprovider.PublishVolumeResponse{Error: providerRefusal(code, "refused: "+string(code))}
	}
}

func unpublishRefusal(code ebsprovider.ErrorCode, message string) ebsprovider.UnpublishVolumeResponse {
	return ebsprovider.UnpublishVolumeResponse{Error: providerRefusal(code, message)}
}

// spyOn records every request sent on subject without answering it, so a
// request the mounter sends there is seen even when another subscriber
// answers it.
func spyOn(t *testing.T, nc *nats.Conn, subject string) func() []contractCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []contractCall
	)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var req ebsprovider.UnpublishVolumeRequest
		_ = json.Unmarshal(msg.Data, &req)
		mu.Lock()
		calls = append(calls, contractCall{
			subject: msg.Subject, version: req.SchemaVersion, volume: req.VolumeID,
			noSeal: req.NoSeal, account: natsmsg.AccountIDFromMsg(msg),
		})
		mu.Unlock()
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return func() []contractCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]contractCall(nil), calls...)
	}
}

// Every mount, unmount and abandon goes to the node's provider over the
// contract at the cluster's one version, and none uses a legacy subject.
func TestVolumeMounterAdapter_EveryRequestAtVersion3OverTheContract(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	provider := newMemoryProvider(t, "vol-a", "vol-b", "vol-hot", "vol-moved")
	serveMemoryProvider(t, nc, provider, node)
	contractCalls := spyOn(t, nc, "ebs.provider.v1."+node+".*")
	var legacySpies []func() []contractCall
	for _, subject := range []string{"ebs." + node + ".*", "ebs.mount", "ebs.unmount", "ebs.abandon"} {
		legacySpies = append(legacySpies, spyOn(t, nc, subject))
	}
	volState := &recordingVolState{}
	adapter := newNATSMounter(nc, node, volState)

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

	moved := mounterTestVM("vol-moved")
	require.NoError(t, adapter.Mount(t.Context(), moved))
	require.NoError(t, adapter.Abandon(t.Context(), moved, "superseded"))
	assert.Equal(t, ebsprovider.VolumeStateAvailable, volumeState(t, provider, "vol-moved"), "abandon must give the export up")

	calls := contractCalls()
	assert.Len(t, calls, 8, "two mounts, a hot attach and detach, two unmounts, a mount and an abandon")
	for _, call := range calls {
		assert.Equal(t, ebsprovider.SchemaVersion, call.version, "%s for %s", call.subject, call.volume)
	}
	for _, legacyCalls := range legacySpies {
		assert.Empty(t, legacyCalls(), "no request may use a legacy subject")
	}
}

// A relaunch refused as retry-later, or because the volume state is not found
// yet, backs off as restore expects and mounts once the provider is ready.
func TestVolumeMounterAdapter_RetryLaterRefusalBacksOffAndRecovers(t *testing.T) {
	for _, code := range []ebsprovider.ErrorCode{ebsprovider.ErrorCodeUnavailable, ebsprovider.ErrorCodeNotFound} {
		t.Run(string(code), func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			fake := serveFakeNodeProvider(t, nc, node)
			fake.setPublish(publishRefusal(code))
			adapter := newNATSMounter(nc, node, nil)

			instance := mounterTestVM("vol-relaunch")
			err := adapter.Mount(t.Context(), instance)
			require.ErrorIs(t, err, vm.ErrMountRetryable, "restore must back off rather than give up")
			hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot"}
			require.ErrorIs(t, adapter.MountOne(t.Context(), testAccountID, hot), vm.ErrMountRetryable)

			fake.setPublish(func(string) ebsprovider.PublishVolumeResponse { return publishedNBD("nbd://ready") })
			require.NoError(t, adapter.Mount(t.Context(), instance))
			assert.Equal(t, "nbd://ready", instance.EBSRequests.Requests[0].NBDURI)
		})
	}
}

// A refusal that retrying cannot fix fails the mount outright, so restore
// does not back off forever against a provider that will never accept it.
func TestVolumeMounterAdapter_PermanentRefusalIsNotRetryable(t *testing.T) {
	codes := []ebsprovider.ErrorCode{
		ebsprovider.ErrorCodeVolumeInUse, ebsprovider.ErrorCodeVolumeHeldElsewhere,
		ebsprovider.ErrorCodeInternal, ebsprovider.ErrorCodeInvalidArgument,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			fake := serveFakeNodeProvider(t, nc, node)
			fake.setPublish(publishRefusal(code))
			adapter := newNATSMounter(nc, node, nil)

			err := adapter.Mount(t.Context(), mounterTestVM("vol-refused"))
			require.Error(t, err)
			assert.NotErrorIs(t, err, vm.ErrMountRetryable)
		})
	}
}

// A volume another node holds is refused with the held-elsewhere code, which
// a provider only sends at version 3, and the refusal reaches Mount's caller
// as the sentinel that tells it apart from an ordinary volume in use.
func TestVolumeMounterAdapter_HeldElsewhereReachesTheMountCaller(t *testing.T) {
	nc := connectMounterTest(t)
	owner, other := newMounterTestNode(), newMounterTestNode()
	provider := newMemoryProvider(t, "vol-held")
	serveMemoryProvider(t, nc, provider, owner)
	serveMemoryProvider(t, nc, provider, other)

	require.NoError(t, newNATSMounter(nc, owner, nil).Mount(t.Context(), mounterTestVM("vol-held")))

	err := newNATSMounter(nc, other, nil).Mount(t.Context(), mounterTestVM("vol-held"))
	require.ErrorIs(t, err, ebsprovider.ErrVolumeHeldElsewhere)
	var providerErr *ebsprovider.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ebsprovider.ErrorCodeVolumeHeldElsewhere, providerErr.Code)
	assert.Equal(t, ebsprovider.VolumeStateInUse, volumeState(t, provider, "vol-held"), "the refused mount must not take the volume from its owner")
}

// The unpublish budget is the provider's data-movement budget, not the
// request timeout every other call gets: a seal that outlasts the request
// timeout still completes, where a publish that slow fails.
func TestVolumeMounterAdapter_UnpublishOutlastingRequestTimeoutCompletes(t *testing.T) {
	const requestTimeout = 100 * time.Millisecond
	const sealTime = 4 * requestTimeout

	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	fake.setUnpublish(func(string) ebsprovider.UnpublishVolumeResponse {
		time.Sleep(sealTime)
		return ebsprovider.UnpublishVolumeResponse{}
	})
	fake.setPublish(func(volumeID string) ebsprovider.PublishVolumeResponse {
		time.Sleep(sealTime)
		return publishedNBD("nbd://" + volumeID)
	})

	volState := &recordingVolState{}
	adapter := newNATSMounter(nc, node, volState)
	adapter.provider = ebsprovider.NewNATSProvider(nc, requestTimeout)

	require.NoError(t, adapter.Unmount(t.Context(), mounterTestVM("vol-slow-seal")))
	assert.Equal(t, []string{"vol-slow-seal:available"}, volState.snapshot())
	require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-slow-seal"}))

	err := adapter.MountOne(t.Context(), testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-slow-seal"})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the publish must be held to the request timeout")
}

// A failed volume rolls back the volumes this Mount already published, and a
// missing volume backs off rather than failing the relaunch.
func TestVolumeMounterAdapter_MountRollsBackOnFailure(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	provider := newMemoryProvider(t, "vol-present")
	serveMemoryProvider(t, nc, provider, node)
	adapter := newNATSMounter(nc, node, nil)

	err := adapter.Mount(t.Context(), mounterTestVM("vol-present", "vol-missing"))
	require.ErrorIs(t, err, vm.ErrMountRetryable)
	assert.ErrorIs(t, err, ebsprovider.ErrNotFound)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, volumeState(t, provider, "vol-present"), "the published volume must be unpublished")
}

// stubMounterProvider publishes a fixed export without a NATS round trip.
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

// A block export replaces whatever export the request carried before, so a
// relaunch never leaves both an NBD URI and a block device set.
func TestVolumeMounterAdapter_BlockExportReplacesNBDURI(t *testing.T) {
	adapter := newVolumeMounterAdapter(&stubMounterProvider{export: ebsprovider.VolumeExport{
		Kind:  ebsprovider.ExportKindBlock,
		Block: &ebsprovider.BlockExport{DevicePath: "/dev/sdx", SizeBytes: 1 << 30},
	}}, "node-stub", nil)

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

// A publish whose outcome is unknown may still have started an export: the
// reply carried no usable export, could not be read, or never came. Hot attach
// gets ErrMountAmbiguous with the request untouched, and Mount tears it down.
func TestVolumeMounterAdapter_AmbiguousPublishIsTornDown(t *testing.T) {
	const requestTimeout = 100 * time.Millisecond
	replies := map[string]struct {
		reply   func() ebsprovider.PublishVolumeResponse
		timeout time.Duration
		cause   error
	}{
		"NoPublishedVolume": {
			reply: func() ebsprovider.PublishVolumeResponse { return ebsprovider.PublishVolumeResponse{} },
			cause: ebsprovider.ErrNoPublishedVolume,
		},
		"EmptyExport": {
			reply: func() ebsprovider.PublishVolumeResponse { return publishedNBD("") },
			cause: ebsprovider.ErrInvalidExport,
		},
		"UnsupportedReplyVersion": {
			reply: func() ebsprovider.PublishVolumeResponse {
				resp := publishedNBD("nbd://future")
				resp.SchemaVersion = ebsprovider.SchemaVersion + 1
				return resp
			},
			cause: ebsprovider.ErrUnsupportedVersion,
		},
		"Timeout": {
			reply: func() ebsprovider.PublishVolumeResponse {
				time.Sleep(4 * requestTimeout)
				return publishedNBD("nbd://late")
			},
			timeout: requestTimeout,
			cause:   context.DeadlineExceeded,
		},
	}
	for name, tt := range replies {
		t.Run(name, func(t *testing.T) {
			nc := connectMounterTest(t)
			node := newMounterTestNode()
			fake := serveFakeNodeProvider(t, nc, node)
			fake.setPublish(func(string) ebsprovider.PublishVolumeResponse { return tt.reply() })
			adapter := newNATSMounter(nc, node, nil)
			if tt.timeout > 0 {
				adapter.provider = ebsprovider.NewNATSProvider(nc, tt.timeout)
			}

			hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot", NBDURI: "nbd://stale"}
			err := adapter.MountOne(t.Context(), testAccountID, hot)
			require.ErrorIs(t, err, vm.ErrMountAmbiguous)
			require.ErrorIs(t, err, tt.cause)
			assert.NotErrorIs(t, err, vm.ErrMountRetryable)
			assert.Equal(t, "nbd://stale", hot.NBDURI, "a failed publish must not overwrite the request's export")

			err = adapter.Mount(t.Context(), mounterTestVM("vol-ambiguous"))
			require.ErrorIs(t, err, vm.ErrMountAmbiguous)
			assert.Equal(t, []string{"vol-ambiguous"}, fake.unpublished(), "the ambiguous volume must be unpublished")
		})
	}
}

// A caller that gives up during a later volume's publish still gets that volume
// and the ones already published unpublished, rather than left exported.
func TestVolumeMounterAdapter_MountRollbackOutlivesCancelledCaller(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	arrived, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake.setPublish(func(volumeID string) ebsprovider.PublishVolumeResponse {
		if volumeID == "vol-slow" {
			close(arrived)
			<-release
		}
		return publishedNBD("nbd://" + volumeID)
	})
	adapter := newNATSMounter(nc, node, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-arrived
		cancel()
	}()
	err := adapter.Mount(ctx, mounterTestVM("vol-first", "vol-slow"))
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, vm.ErrMountAmbiguous)
	assert.NotErrorIs(t, err, vm.ErrMountRetryable)
	assert.ElementsMatch(t, []string{"vol-first", "vol-slow"}, fake.unpublished(),
		"the published volume and the one whose publish the provider received must be unpublished despite the cancelled caller")
}

// A hot attach whose caller gives up after the provider received the publish
// is ambiguous, so the attach unpublishes it, and that teardown still reaches
// the provider under the cancelled context.
func TestVolumeMounterAdapter_MountOneCancelledAfterSendIsAmbiguous(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	arrived, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake.setPublish(func(volumeID string) ebsprovider.PublishVolumeResponse {
		close(arrived)
		<-release
		return publishedNBD("nbd://" + volumeID)
	})
	adapter := newNATSMounter(nc, node, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-arrived
		cancel()
	}()
	req := &viperblocklegacyv1.EBSRequest{Name: "vol-hot", NBDURI: "nbd://stale"}
	err := adapter.MountOne(ctx, testAccountID, req)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, vm.ErrMountAmbiguous)
	assert.NotErrorIs(t, err, vm.ErrMountRetryable)
	assert.Equal(t, "nbd://stale", req.NBDURI, "a failed publish must not overwrite the request's export")

	require.NoError(t, adapter.UnmountOne(ctx, testAccountID, *req))
	assert.Equal(t, []string{"vol-hot"}, fake.unpublished())
}

// A caller already gone when a publish would start sends nothing, so neither
// call is ambiguous and Mount unpublishes only the volumes it published.
func TestVolumeMounterAdapter_PublishAfterCallerGoneSendsNothing(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	adapter := newNATSMounter(nc, node, nil)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	err := adapter.MountOne(cancelled, testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-hot"})
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, vm.ErrMountAmbiguous)

	err = adapter.Mount(cancelled, mounterTestVM("vol-never"))
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, vm.ErrMountAmbiguous)
	assert.Empty(t, fake.snapshot(), "no request may reach the provider for a caller already gone")
}

// Every contract request names the account it acts for on the wire, so the
// provider's server span is attributable, and an unattributed one names none.
func TestVolumeMounterAdapter_ContractRequestsCarryTheAccount(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	serveFakeNodeProvider(t, nc, node)
	calls := spyOn(t, nc, "ebs.provider.v1."+node+".*")
	adapter := newNATSMounter(nc, node, nil)

	instance := mounterTestVM("vol-a")
	require.NoError(t, adapter.Mount(t.Context(), instance))
	require.NoError(t, adapter.Unmount(t.Context(), instance))
	require.NoError(t, adapter.Abandon(t.Context(), instance, "superseded"))
	hot := &viperblocklegacyv1.EBSRequest{Name: "vol-hot"}
	require.NoError(t, adapter.MountOne(t.Context(), testAccountID, hot))
	require.NoError(t, adapter.UnmountOne(t.Context(), testAccountID, *hot))
	require.NoError(t, adapter.MountOne(t.Context(), "", &viperblocklegacyv1.EBSRequest{Name: "vol-none"}))

	require.Eventually(t, func() bool { return len(calls()) == 6 }, 5*time.Second, 10*time.Millisecond)
	for _, call := range calls() {
		want := testAccountID
		if call.volume == "vol-none" {
			want = ""
		}
		assert.Equal(t, want, call.account, "%s for %s", call.subject, call.volume)
	}
}

// The provider's request span carries the account under the caller's trace,
// with no mounter span of its own in between.
func TestVolumeMounterAdapter_RequestSpanCarriesTheAccountUnderTheCaller(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	serveFakeNodeProvider(t, nc, node)
	adapter := newNATSMounter(nc, node, nil)

	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	require.NoError(t, adapter.MountOne(ctx, testAccountID, &viperblocklegacyv1.EBSRequest{Name: "vol-acct"}))
	caller.End()

	request := clientSpan(t, sr)
	assert.Equal(t, "ebs.volume.publish", request.Name())
	assert.Equal(t, testAccountID, spanAccountOf(request), "the request span must name the account it acted for")
	assert.Equal(t, caller.SpanContext().SpanID(), request.Parent().SpanID(), "the request span must join the caller directly")
}

// A detach spends part of its deadline on the unplug, so the seal must not
// inherit what is left: a caller context already cancelled, or one shorter
// than the seal, still gets the seal completed.
func TestVolumeMounterAdapter_UnpublishOutlivesCallerContext(t *testing.T) {
	const sealTime = 200 * time.Millisecond

	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	var sealed atomic.Int32
	fake.setUnpublish(func(string) ebsprovider.UnpublishVolumeResponse {
		time.Sleep(sealTime)
		sealed.Add(1)
		return ebsprovider.UnpublishVolumeResponse{}
	})
	volState := &recordingVolState{}
	adapter := newNATSMounter(nc, node, volState)

	short, cancelShort := context.WithTimeout(t.Context(), sealTime/4)
	defer cancelShort()
	require.NoError(t, adapter.UnmountOne(short, testAccountID, viperblocklegacyv1.EBSRequest{Name: "vol-detach"}))

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, adapter.Unmount(cancelled, mounterTestVM("vol-stop")))
	assert.Equal(t, int32(2), sealed.Load())
	assert.Equal(t, []string{"vol-stop:available"}, volState.snapshot())
}

// Abandon drops each export unsealed, at the version that carries NoSeal, and
// tries every volume: an export torn down is logged, one not held here or
// unknown to the provider is the ordinary case, and a refusal is returned.
func TestVolumeMounterAdapter_AbandonUnpublishesWithoutSealing(t *testing.T) {
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	fake := serveFakeNodeProvider(t, nc, node)
	fake.setUnpublish(func(volumeID string) ebsprovider.UnpublishVolumeResponse {
		switch volumeID {
		case "vol-torn":
			return ebsprovider.UnpublishVolumeResponse{TornDown: true}
		case "vol-unknown":
			return unpublishRefusal(ebsprovider.ErrorCodeNotFound, "volume not found")
		case "vol-refused":
			return unpublishRefusal(ebsprovider.ErrorCodeUnavailable, "export busy")
		}
		return ebsprovider.UnpublishVolumeResponse{}
	})
	logs := captureSlogForTest(t)
	adapter := newNATSMounter(nc, node, nil)

	err := adapter.Abandon(t.Context(), mounterTestVM("vol-torn", "vol-absent", "vol-unknown", "vol-refused"), "superseded")
	require.ErrorIs(t, err, ebsprovider.ErrUnavailable)
	assert.Contains(t, err.Error(), "abandon vol-refused")
	for _, volume := range []string{"vol-torn", "vol-absent", "vol-unknown"} {
		assert.NotContains(t, err.Error(), volume)
	}

	calls := fake.snapshot()
	require.Len(t, calls, 4, "every volume is attempted")
	for _, call := range calls {
		assert.True(t, call.noSeal, "%s must be dropped unsealed", call.volume)
		assert.Equal(t, ebsprovider.NoSealSchemaVersion, call.version, "%s must be sent at the version carrying NoSeal", call.volume)
	}
	assert.Contains(t, err.Error(), "abandon vol-refused (superseded)", "the failure must say why the volume was abandoned")
	const gaveUp = "Gave up a volume export"
	assert.Contains(t, logs.String(), "instance=i-mounter volume=vol-torn reason=superseded")
	assert.Equal(t, 1, strings.Count(logs.String(), gaveUp), "only a torn-down export is reported as given up")
}

// An abandon that finds no provider has not let go, so the caller is told.
func TestVolumeMounterAdapter_AbandonNoResponderFails(t *testing.T) {
	nc := connectMounterTest(t)
	adapter := newNATSMounter(nc, newMounterTestNode(), nil)

	err := adapter.Abandon(t.Context(), mounterTestVM("vol-1", "vol-2"), "superseded")
	require.ErrorIs(t, err, nats.ErrNoResponders)
	assert.Contains(t, err.Error(), "vol-1")
	assert.Contains(t, err.Error(), "vol-2", "every volume is attempted")
}

// An abandon that is never answered gives up within its own bound rather
// than the provider's data-movement budget for an unpublish.
func TestVolumeMounterAdapter_AbandonGivesUpWithinItsBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	spyOn(t, nc, "ebs.provider.v1."+node+".*")
	adapter := newNATSMounter(nc, node, nil)
	adapter.abandonTimeout = bound

	done := make(chan error, 1)
	go func() { done <- adapter.Abandon(t.Context(), mounterTestVM("vol-silent"), "superseded") }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(20 * bound):
		t.Fatal("abandon outlasted its bound")
	}
}

// The abandon span names the instance, the account and why it was abandoned,
// and the provider's request spans sit under it.
func TestVolumeMounterAdapter_AbandonSpanRecordsTheReason(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	serveFakeNodeProvider(t, nc, node)
	adapter := newNATSMounter(nc, node, nil)

	require.NoError(t, adapter.Abandon(t.Context(), mounterTestVM("vol-moved"), "superseded"))

	request := clientSpan(t, sr)
	var abandon sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "ebs.mounter.abandon" {
			abandon = s
		}
	}
	require.NotNil(t, abandon)
	assert.Equal(t, "superseded", spanAttribute(abandon, "abandon.reason"))
	assert.Equal(t, "i-mounter", spanAttribute(abandon, "instance.id"))
	assert.Equal(t, testAccountID, spanAccountOf(abandon))
	assert.Equal(t, abandon.SpanContext().SpanID(), request.Parent().SpanID())
}
