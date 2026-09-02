package natsserve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const requestTimeout = 2 * time.Second

// wireEnvelope decodes the two fields every response in the contract carries,
// so one type reads the reply of any subject in the table below.
type wireEnvelope struct {
	ebsprovider.Versioned

	Error *ebsprovider.ProviderError `json:"error"`
}

func decodeEnvelope(t *testing.T, data []byte) wireEnvelope {
	t.Helper()
	var envelope wireEnvelope
	require.NoError(t, json.Unmarshal(data, &envelope))
	return envelope
}

// serveProvider starts a server over its own NATS connection and returns the
// connection, so a test can drive raw payloads the typed client would refuse
// to send.
func serveProvider(t *testing.T, provider ebsprovider.EBSProvider, opts natsserve.Options) *nats.Conn {
	t.Helper()
	_, conn := testutil.StartTestNATS(t)
	stop, err := natsserve.Serve(t.Context(), conn, provider, opts)
	require.NoError(t, err)
	t.Cleanup(stop)
	return conn
}

// failingProvider fails every verb with one wrapped sentinel, so a handler's
// classifyError arm can be reached without contriving provider state.
type failingProvider struct{ err error }

var _ ebsprovider.EBSProvider = (*failingProvider)(nil)

func (p *failingProvider) GetCapabilities(context.Context, ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	return nil, p.err
}

func (p *failingProvider) CreateVolume(context.Context, ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	return nil, p.err
}

func (p *failingProvider) GetVolume(context.Context, ebsprovider.GetVolumeRequest) (*ebsprovider.Volume, error) {
	return nil, p.err
}

func (p *failingProvider) ListVolumes(context.Context, ebsprovider.ListVolumesRequest) (*ebsprovider.ListVolumesResponse, error) {
	return nil, p.err
}

func (p *failingProvider) ExpandVolume(context.Context, ebsprovider.ExpandVolumeRequest) (*ebsprovider.Volume, error) {
	return nil, p.err
}

func (p *failingProvider) DeleteVolume(context.Context, ebsprovider.DeleteVolumeRequest) error {
	return p.err
}

func (p *failingProvider) CreateSnapshot(context.Context, ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	return nil, p.err
}

func (p *failingProvider) DeleteSnapshot(context.Context, ebsprovider.DeleteSnapshotRequest) error {
	return p.err
}

func (p *failingProvider) CopySnapshot(context.Context, ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	return nil, p.err
}

func (p *failingProvider) ListSnapshots(context.Context, ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	return nil, p.err
}

func (p *failingProvider) PublishVolume(context.Context, ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	return nil, p.err
}

func (p *failingProvider) UnpublishVolume(context.Context, ebsprovider.UnpublishVolumeRequest) error {
	return p.err
}

// wireVerb names one subject Serve subscribes, a valid request body for it,
// and — for the one asynchronous verb — the subject its provider failure is
// published to rather than returned in the reply.
type wireVerb struct {
	name              string
	subject           string
	request           any
	completionSubject string
}

func wireVerbs(t *testing.T) []wireVerb {
	t.Helper()
	snapshotCreateSubject, err := ebsprovider.SnapshotSubject("vol-1")
	require.NoError(t, err)
	completionSubject, err := ebsprovider.SnapshotCompletionSubject("snap-1")
	require.NoError(t, err)
	mountSubject, err := ebsprovider.PublishSubject("node-1")
	require.NoError(t, err)
	unmountSubject, err := ebsprovider.UnpublishSubject("node-1")
	require.NoError(t, err)

	return []wireVerb{
		{name: "capabilities", subject: ebsprovider.CapabilitiesSubject,
			request: ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()}},
		{name: "volume.create", subject: ebsprovider.CreateVolumeSubject,
			request: ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}}},
		{name: "volume.describe", subject: ebsprovider.GetVolumeSubject,
			request: ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1"}},
		{name: "volume.list", subject: ebsprovider.ListVolumesSubject,
			request: ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()}},
		{name: "volume.expand", subject: ebsprovider.ExpandVolumeSubject,
			request: ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}}},
		{name: "volume.delete", subject: ebsprovider.DeleteVolumeSubject,
			request: ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1"}},
		{name: "snapshot.delete", subject: ebsprovider.DeleteSnapshotSubject,
			request: ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-1"}},
		{name: "snapshot.copy", subject: ebsprovider.CopySnapshotSubject,
			request: ebsprovider.CopySnapshotRequest{Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-1", DestinationSnapshotID: "snap-2", VolumeID: "vol-1"}},
		{name: "snapshot.list", subject: ebsprovider.ListSnapshotsSubject,
			request: ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.NewVersioned()}},
		{name: "snapshot.create", subject: snapshotCreateSubject, completionSubject: completionSubject,
			request: ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-1", VolumeID: "vol-1"}},
		{name: "mount", subject: mountSubject,
			request: ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1", NodeID: "node-1"}},
		{name: "unmount", subject: unmountSubject,
			request: ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1", NodeID: "node-1"}},
	}
}

// TestHandlersRejectMalformedRequests pins the caller-fault arm of every
// subject: a body that is not JSON must come back as a versioned
// invalid_argument reply, not as a dropped message the caller waits out.
func TestHandlersRejectMalformedRequests(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})

	for _, verb := range wireVerbs(t) {
		t.Run(verb.name, func(t *testing.T) {
			msg, err := conn.Request(verb.subject, []byte("{not json"), requestTimeout)
			require.NoError(t, err)

			envelope := decodeEnvelope(t, msg.Data)
			assert.Equal(t, ebsprovider.SchemaVersion, envelope.SchemaVersion)
			require.NotNil(t, envelope.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, envelope.Error.Code)
			assert.True(t, strings.HasPrefix(envelope.Error.Message, "bad request: "), "got %q", envelope.Error.Message)
		})
	}
}

// TestHandlersRejectVersionSkew is the guard the whole Versioned embedding
// exists for: a peer speaking a different schema must be refused at the
// boundary rather than have its payload decoded into this build's types.
func TestHandlersRejectVersionSkew(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})

	for _, verb := range wireVerbs(t) {
		t.Run(verb.name, func(t *testing.T) {
			msg, err := conn.Request(verb.subject, []byte(`{"schema_version":99}`), requestTimeout)
			require.NoError(t, err)

			envelope := decodeEnvelope(t, msg.Data)
			assert.Equal(t, ebsprovider.SchemaVersion, envelope.SchemaVersion, "the refusal itself must be versioned")
			require.NotNil(t, envelope.Error)
			assert.Equal(t, ebsprovider.ErrorCodeUnsupportedVersion, envelope.Error.Code)
			assert.Equal(t, fmt.Sprintf("unsupported schema version 99, want %d", ebsprovider.SchemaVersion), envelope.Error.Message)
		})
	}
}

// TestHandlersClassifyProviderFailures proves each handler carries the
// provider's sentinel across the wire rather than flattening it to internal:
// a caller switching on ErrorCodeUnavailable to decide whether to retry gets
// the wrong answer from any subject that loses it.
func TestHandlersClassifyProviderFailures(t *testing.T) {
	provider := &failingProvider{err: fmt.Errorf("vol-1: %w", ebsprovider.ErrUnavailable)}
	conn := serveProvider(t, provider, natsserve.Options{NoQueueGroup: true})

	for _, verb := range wireVerbs(t) {
		t.Run(verb.name, func(t *testing.T) {
			payload, err := json.Marshal(verb.request)
			require.NoError(t, err)

			// The asynchronous create replies "accepted" before it calls the
			// provider, so its failure arrives on the completion subject.
			var completionSub *nats.Subscription
			if verb.completionSubject != "" {
				completionSub, err = conn.SubscribeSync(verb.completionSubject)
				require.NoError(t, err)
				t.Cleanup(func() { _ = completionSub.Unsubscribe() })
				require.NoError(t, conn.Flush())
			}

			msg, err := conn.Request(verb.subject, payload, requestTimeout)
			require.NoError(t, err)
			if completionSub != nil {
				msg, err = completionSub.NextMsg(requestTimeout)
				require.NoError(t, err)
			}

			envelope := decodeEnvelope(t, msg.Data)
			require.NotNil(t, envelope.Error)
			assert.Equal(t, ebsprovider.ErrorCodeUnavailable, envelope.Error.Code)
			assert.Equal(t, "vol-1: "+ebsprovider.ErrUnavailable.Error(), envelope.Error.Message, "the wrapping context must survive the wire")
			assert.True(t, envelope.Error.Code.Retryable(), "a transient failure must still invite a retry after crossing the wire")
		})
	}
}

// TestServedProviderRoundTrip drives every verb through NATSProvider against a
// served MemoryProvider, so the client's encoding and the server's decoding are
// asserted against each other rather than each against a hand-written payload.
func TestServedProviderRoundTrip(t *testing.T) {
	capabilities := ebsprovider.Capabilities{
		VolumeEnumeration:   true,
		SnapshotEnumeration: true,
		OnlineExpansion:     true,
	}
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(capabilities), natsserve.Options{NoQueueGroup: true})
	client := ebsprovider.NewNATSProvider(conn, requestTimeout)
	ctx := t.Context()

	caps, err := client.GetCapabilities(ctx, ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	assert.True(t, caps.Capabilities.VolumeEnumeration)
	assert.Equal(t, ebsprovider.ExclusionScopeNode, caps.Capabilities.Exclusion.Scope)

	volume, err := client.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4 << 30}, AvailabilityZone: "ap-southeast-2a",
	})
	require.NoError(t, err)
	assert.Equal(t, "memory://volume/vol-roundtrip", volume.Handle)
	assert.Equal(t, "ap-southeast-2a", volume.AvailabilityZone)

	described, err := client.GetVolume(ctx, ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip", Handle: volume.Handle,
	})
	require.NoError(t, err)
	assert.Equal(t, volume, described)

	listed, err := client.ListVolumes(ctx, ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	assert.Equal(t, []ebsprovider.VolumeRef{{ID: "vol-roundtrip", Handle: volume.Handle}}, listed.Volumes)
	assert.Empty(t, listed.NextToken)

	expanded, err := client.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 8 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(8<<30), expanded.CapacityBytes)

	published, err := client.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip", NodeID: "node-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "nbd+unix:///?socket=/memory/vol-roundtrip.sock", published.NBDURI)

	inUse, err := client.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip"})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.VolumeStateInUse, inUse.State)

	require.NoError(t, client.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip", NodeID: "node-1",
	}))
	available, err := client.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip"})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.VolumeStateAvailable, available.State)

	snapshot, err := client.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-roundtrip", VolumeID: "vol-roundtrip",
	})
	require.NoError(t, err)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snapshot.State, "the pending reply must be superseded by the completion event")
	assert.Equal(t, int64(8<<30), snapshot.SizeBytes)

	copied, err := client.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-roundtrip",
		DestinationSnapshotID: "snap-copy", VolumeID: "vol-roundtrip",
	})
	require.NoError(t, err)
	assert.Equal(t, "vol-roundtrip", copied.SourceVolumeID)
	assert.Equal(t, snapshot.SizeBytes, copied.SizeBytes)

	snapshots, err := client.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	assert.Equal(t, []ebsprovider.SnapshotRef{
		{ID: "snap-copy", SourceVolumeID: "vol-roundtrip", Handle: "memory://snapshot/snap-copy"},
		{ID: "snap-roundtrip", SourceVolumeID: "vol-roundtrip", Handle: "memory://snapshot/snap-roundtrip"},
	}, snapshots.Snapshots)

	require.NoError(t, client.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copy"}))
	require.NoError(t, client.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip"}))

	_, err = client.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-roundtrip"})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound)
}

// TestCreateSnapshotAcceptsBeforeCompleting pins the two-stage shape of the
// asynchronous create: the immediate reply must name a pending snapshot, an
// operation ID and the completion subject the caller is expected to already
// be subscribed to, since a reply that omitted any of them would leave the
// caller waiting on a subject nothing will ever publish to.
func TestCreateSnapshotAcceptsBeforeCompleting(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})
	ctx := t.Context()

	provider := ebsprovider.NewNATSProvider(conn, requestTimeout)
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-async", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)

	subject, err := ebsprovider.SnapshotSubject("vol-async")
	require.NoError(t, err)
	completionSubject, err := ebsprovider.SnapshotCompletionSubject("snap-async")
	require.NoError(t, err)
	completionSub, err := conn.SubscribeSync(completionSubject)
	require.NoError(t, err)
	t.Cleanup(func() { _ = completionSub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	payload, err := json.Marshal(ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-async", VolumeID: "vol-async",
	})
	require.NoError(t, err)
	msg, err := conn.Request(subject, payload, requestTimeout)
	require.NoError(t, err)

	var accepted ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(msg.Data, &accepted))
	require.Nil(t, accepted.Error)
	assert.Equal(t, completionSubject, accepted.CompletionSubject)
	assert.NotEmpty(t, accepted.OperationID)
	require.NotNil(t, accepted.Snapshot)
	assert.Equal(t, ebsprovider.SnapshotStatePending, accepted.Snapshot.State)
	assert.Equal(t, "vol-async", accepted.Snapshot.SourceVolumeID)

	completionMsg, err := completionSub.NextMsg(requestTimeout)
	require.NoError(t, err)
	var completed ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(completionMsg.Data, &completed))
	require.Nil(t, completed.Error)
	assert.Equal(t, accepted.OperationID, completed.OperationID, "the completion must be attributable to the operation it finishes")
	require.NotNil(t, completed.Snapshot)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, completed.Snapshot.State)
}

// TestCreateSnapshotRejectsSubjectBodyMismatch covers the wildcard's own
// check: the subject names the volume the request is routed by, so a body
// naming a different one would snapshot a volume the router never selected.
func TestCreateSnapshotRejectsSubjectBodyMismatch(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})

	subject, err := ebsprovider.SnapshotSubject("vol-subject")
	require.NoError(t, err)
	payload, err := json.Marshal(ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-1", VolumeID: "vol-body",
	})
	require.NoError(t, err)

	msg, err := conn.Request(subject, payload, requestTimeout)
	require.NoError(t, err)

	envelope := decodeEnvelope(t, msg.Data)
	require.NotNil(t, envelope.Error)
	assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, envelope.Error.Code)
	assert.Equal(t, `volume id "vol-subject" in subject does not match request volume id "vol-body"`, envelope.Error.Message)
}

// TestCreateSnapshotRejectsUnroutableSnapshotID covers the completion-subject
// build failure: a snapshot ID containing a NATS wildcard has no addressable
// completion subject, so the request must be refused rather than accepted
// into a background operation whose result can never be delivered.
func TestCreateSnapshotRejectsUnroutableSnapshotID(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})

	subject, err := ebsprovider.SnapshotSubject("vol-1")
	require.NoError(t, err)
	payload, err := json.Marshal(ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap.*", VolumeID: "vol-1",
	})
	require.NoError(t, err)

	msg, err := conn.Request(subject, payload, requestTimeout)
	require.NoError(t, err)

	envelope := decodeEnvelope(t, msg.Data)
	require.NotNil(t, envelope.Error)
	assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, envelope.Error.Code)
	assert.Contains(t, envelope.Error.Message, "invalid NATS subject token")
}

// countingProvider records how many times each subject reached a provider, so
// a subscription's delivery semantics can be asserted from the provider side.
type countingProvider struct {
	ebsprovider.EBSProvider

	capabilities atomic.Int32
	publishes    atomic.Int32
}

func (p *countingProvider) GetCapabilities(ctx context.Context, req ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	p.capabilities.Add(1)
	return p.EBSProvider.GetCapabilities(ctx, req)
}

func (p *countingProvider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	p.publishes.Add(1)
	return p.EBSProvider.PublishVolume(ctx, req)
}

// TestServeQueueGroupBalancesSharedSubjects is the difference between two
// workers sharing a workload and both doing all of it. The mount subject is
// never queue-grouped, so once both servers have handled a mount, message
// ordering on the shared connection guarantees the earlier capabilities
// request has already been delivered to everyone that was going to get it.
func TestServeQueueGroupBalancesSharedSubjects(t *testing.T) {
	for _, tc := range []struct {
		name         string
		opts         natsserve.Options
		wantHandlers int32
	}{
		{name: "queue group", opts: natsserve.Options{}, wantHandlers: 1},
		{name: "no queue group", opts: natsserve.Options{NoQueueGroup: true}, wantHandlers: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, conn := testutil.StartTestNATS(t)
			workers := []*countingProvider{
				{EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})},
				{EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})},
			}
			for _, worker := range workers {
				stop, err := natsserve.Serve(t.Context(), conn, worker, tc.opts)
				require.NoError(t, err)
				t.Cleanup(stop)
			}

			client := ebsprovider.NewNATSProvider(conn, requestTimeout)
			_, err := client.GetCapabilities(t.Context(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
			require.NoError(t, err)

			_, err = client.PublishVolume(t.Context(), ebsprovider.PublishVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-absent", NodeID: "node-1",
			})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
			require.Eventually(t, func() bool {
				return workers[0].publishes.Load()+workers[1].publishes.Load() == 2
			}, requestTimeout, 5*time.Millisecond, "mount is never queue-grouped, so both servers must see it")

			assert.Equal(t, tc.wantHandlers, workers[0].capabilities.Load()+workers[1].capabilities.Load())
		})
	}
}

// TestServeNodeIDScopesMountSubjects covers the production shape: a daemon that
// can only mount on its own node must not answer another node's mount subject,
// or a volume gets attached on a host that cannot see the guest asking for it.
func TestServeNodeIDScopesMountSubjects(t *testing.T) {
	conn := serveProvider(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NodeID: "node-1"})

	payload, err := json.Marshal(ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1", NodeID: "node-1",
	})
	require.NoError(t, err)

	ownSubject, err := ebsprovider.PublishSubject("node-1")
	require.NoError(t, err)
	msg, err := conn.Request(ownSubject, payload, requestTimeout)
	require.NoError(t, err)
	envelope := decodeEnvelope(t, msg.Data)
	require.NotNil(t, envelope.Error)
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, envelope.Error.Code, "the server answered its own subject")

	otherSubject, err := ebsprovider.PublishSubject("node-2")
	require.NoError(t, err)
	_, err = conn.Request(otherSubject, payload, requestTimeout)
	require.ErrorIs(t, err, nats.ErrNoResponders, "another node's mount subject must go unanswered")
}

// TestServeRejectsUnroutableNodeID covers the subject-build failure path: an
// unsubscribable node ID must fail Serve outright rather than leave a server
// running with the shared subjects subscribed and no mount subject at all.
func TestServeRejectsUnroutableNodeID(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)

	stop, err := natsserve.Serve(t.Context(), conn, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NodeID: "node.*"})
	require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
	assert.Contains(t, err.Error(), "build publish subject")
	assert.Nil(t, stop)

	_, err = conn.Request(ebsprovider.CapabilitiesSubject, []byte(`{"schema_version":1}`), 200*time.Millisecond)
	require.ErrorIs(t, err, nats.ErrNoResponders, "a failed Serve must leave no subscription behind")
}

// TestServeFailsOnClosedConnection covers the subscribe error path for the
// shared subjects, which is the first thing Serve does.
func TestServeFailsOnClosedConnection(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	conn.Close()

	stop, err := natsserve.Serve(t.Context(), conn, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})
	require.ErrorIs(t, err, nats.ErrConnectionClosed)
	assert.Contains(t, err.Error(), "subscribe to "+ebsprovider.CapabilitiesSubject)
	assert.Nil(t, stop)
}

// TestServeStopIsIdempotent covers the sync.Once in the returned stop: the
// documented contract is that calling it twice is safe, and a caller with both
// a defer and an explicit shutdown path does exactly that.
func TestServeStopIsIdempotent(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	stop, err := natsserve.Serve(t.Context(), conn, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), natsserve.Options{NoQueueGroup: true})
	require.NoError(t, err)

	stop()
	stop()
	require.NoError(t, conn.Flush())

	_, err = conn.Request(ebsprovider.CapabilitiesSubject, []byte(`{"schema_version":1}`), 200*time.Millisecond)
	require.ErrorIs(t, err, nats.ErrNoResponders, "stop must unsubscribe every subject Serve registered")
}
