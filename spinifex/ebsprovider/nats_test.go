package ebsprovider

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNATSProviderCreateVolumeUsesVersionedContract(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	sub, err := conn.Subscribe(CreateVolumeSubject, func(msg *nats.Msg) {
		var request CreateVolumeRequest
		require.NoError(t, json.Unmarshal(msg.Data, &request))
		assert.Equal(t, SchemaVersion, request.SchemaVersion)
		assert.Equal(t, "vol-1", request.VolumeID)
		response, marshalErr := json.Marshal(CreateVolumeResponse{
			Versioned: NewVersioned(),
			Volume:    &Volume{ID: request.VolumeID, CapacityBytes: request.CapacityRange.RequiredBytes, Handle: "vb://vol-1"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(response))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	volume, err := provider.CreateVolume(t.Context(), CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-1", CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, "vb://vol-1", volume.Handle)
}

func TestNATSProviderRejectsResponseVersionSkew(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	sub, err := conn.Subscribe(GetVolumeSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetVolumeResponse{
			Versioned: Versioned{SchemaVersion: SchemaVersion + 1},
			Volume:    &Volume{ID: "vol-1"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	_, err = provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestNATSProviderReturnsTypedProviderError(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	sub, err := conn.Subscribe(DeleteVolumeSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(DeleteVolumeResponse{
			Versioned: NewVersioned(),
			Error:     &ProviderError{Code: ErrorCodeVolumeInUse, Message: "volume is mounted"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	err = provider.DeleteVolume(t.Context(), DeleteVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrVolumeInUse)
	assert.Equal(t, "volume is mounted", err.Error())
}

func TestNATSProviderSnapshotWaitsForAsyncCompletion(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	subject, err := SnapshotSubject("vol-1")
	require.NoError(t, err)
	completionSubject, err := SnapshotCompletionSubject("snap-1")
	require.NoError(t, err)

	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		accepted, marshalErr := json.Marshal(CreateSnapshotResponse{
			Versioned: NewVersioned(), OperationID: "op-snap-1", CompletionSubject: completionSubject,
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(accepted))

		completed, marshalErr := json.Marshal(CreateSnapshotResponse{
			Versioned: NewVersioned(), OperationID: "op-snap-1",
			Snapshot: &Snapshot{ID: "snap-1", SourceVolumeID: "vol-1", State: SnapshotStateCompleted},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, conn.Publish(completionSubject, completed))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	snapshot, err := provider.CreateSnapshot(t.Context(), CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: "vol-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "snap-1", snapshot.ID)
}

func TestNATSProviderRejectsUnsafeSubjectTokens(t *testing.T) {
	provider := NewNATSProvider(nil, time.Second)
	_, err := provider.PublishVolume(t.Context(), PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node.*",
	})
	require.ErrorIs(t, err, ErrInvalidArgument)
	assert.NotErrorIs(t, err, nats.ErrConnectionClosed, "subject validation must happen before transport use")
}

// TestSubjectBuildersRejectUnsafeTokens tables every NATS subject-token
// validation across the four functions that embed a caller-supplied ID in a
// subject: empty, ".", "..", and any token containing a NATS wildcard
// character must be rejected before a subject string is ever built.
func TestSubjectBuildersRejectUnsafeTokens(t *testing.T) {
	badTokens := []string{"", ".", "..", "a.b", "a*b", "a>b"}

	builders := []struct {
		name  string
		build func(string) (string, error)
	}{
		{"SnapshotSubject", SnapshotSubject},
		{"SnapshotCompletionSubject", SnapshotCompletionSubject},
		{"PublishSubject", PublishSubject},
		{"UnpublishSubject", UnpublishSubject},
	}

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			for _, token := range badTokens {
				t.Run(fmt.Sprintf("%q", token), func(t *testing.T) {
					_, err := b.build(token)
					require.ErrorIs(t, err, ErrInvalidArgument)
				})
			}
			t.Run("valid token", func(t *testing.T) {
				subject, err := b.build("node-1")
				require.NoError(t, err)
				assert.NotEmpty(t, subject)
			})
		})
	}
}

// TestNATSProviderChecksVersionBeforeTransport confirms every EBSProvider
// method on NATSProvider rejects an unsupported SchemaVersion via
// checkVersion before it ever touches the connection. A nil *nats.Conn would
// panic or return nats.ErrConnectionClosed if any method reached transport,
// so seeing ErrUnsupportedVersion here proves the ordering.
func TestNATSProviderChecksVersionBeforeTransport(t *testing.T) {
	provider := NewNATSProvider(nil, time.Second)
	ctx := t.Context()

	t.Run("GetCapabilities", func(t *testing.T) {
		_, err := provider.GetCapabilities(ctx, GetCapabilitiesRequest{})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("CreateVolume", func(t *testing.T) {
		_, err := provider.CreateVolume(ctx, CreateVolumeRequest{VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("GetVolume", func(t *testing.T) {
		_, err := provider.GetVolume(ctx, GetVolumeRequest{VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("ExpandVolume", func(t *testing.T) {
		_, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("DeleteVolume", func(t *testing.T) {
		err := provider.DeleteVolume(ctx, DeleteVolumeRequest{VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("CreateSnapshot", func(t *testing.T) {
		_, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{SnapshotID: "snap-1", VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("DeleteSnapshot", func(t *testing.T) {
		err := provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{SnapshotID: "snap-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("PublishVolume", func(t *testing.T) {
		_, err := provider.PublishVolume(ctx, PublishVolumeRequest{VolumeID: "vol-1", NodeID: "node-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
	t.Run("UnpublishVolume", func(t *testing.T) {
		err := provider.UnpublishVolume(ctx, UnpublishVolumeRequest{VolumeID: "vol-1", NodeID: "node-1"})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
}

// TestNewNATSProviderDefaultsTimeout covers the requestTimeout<=0 branch:
// callers passing a zero or negative timeout get defaultRequestTimeout
// rather than a provider that never times out.
func TestNewNATSProviderDefaultsTimeout(t *testing.T) {
	assert.Equal(t, defaultRequestTimeout, NewNATSProvider(nil, 0).requestTimeout)
	assert.Equal(t, defaultRequestTimeout, NewNATSProvider(nil, -time.Second).requestTimeout)
	assert.Equal(t, 7*time.Second, NewNATSProvider(nil, 7*time.Second).requestTimeout)
}

// TestNATSProviderGetCapabilities_TransportError covers the p.request error
// branch (no responder on the subject) for a method that otherwise never
// exercises it elsewhere in this suite.
func TestNATSProviderGetCapabilities_TransportError(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	provider := NewNATSProvider(conn, 200*time.Millisecond)
	_, err := provider.GetCapabilities(t.Context(), GetCapabilitiesRequest{Versioned: NewVersioned()})
	require.Error(t, err)
}

// TestNATSProviderGetCapabilities_ProviderError covers responseError's
// populated-Error branch for GetCapabilities specifically.
func TestNATSProviderGetCapabilities_ProviderError(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	sub, err := conn.Subscribe(CapabilitiesSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetCapabilitiesResponse{
			Versioned: NewVersioned(), Error: &ProviderError{Code: ErrorCodeInternal, Message: "capabilities unavailable"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	_, err = provider.GetCapabilities(t.Context(), GetCapabilitiesRequest{Versioned: NewVersioned()})
	require.EqualError(t, err, "capabilities unavailable")
}

// TestNATSProviderRequest covers p.request's three error branches directly:
// an unmarshalable input, a subject with no responder, and a responder that
// answers with invalid JSON.
func TestNATSProviderRequest(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	provider := NewNATSProvider(conn, 200*time.Millisecond)

	t.Run("marshal error", func(t *testing.T) {
		var out GetVolumeResponse
		err := provider.request(t.Context(), "ebs.provider.v1.test.marshal", make(chan int), &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "encode")
	})

	t.Run("no responder", func(t *testing.T) {
		var out GetVolumeResponse
		err := provider.request(t.Context(), "ebs.provider.v1.test.noresponder", GetVolumeRequest{Versioned: NewVersioned()}, &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "request")
	})

	t.Run("decode error", func(t *testing.T) {
		const subject = "ebs.provider.v1.test.baddecode"
		sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
			require.NoError(t, msg.Respond([]byte("{not valid json")))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		require.NoError(t, conn.Flush())

		var out GetVolumeResponse
		err = provider.request(t.Context(), subject, GetVolumeRequest{Versioned: NewVersioned()}, &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode")
	})
}

// TestNATSProviderOwnerFirst_RoutesToOwnerSubject proves the owner subject
// is tried first: only a responder on GetVolumeOwnerSubject exists (no
// GetVolumeSubject responder at all), so a successful reply proves the
// request never needed the queue-group subject.
func TestNATSProviderOwnerFirst_RoutesToOwnerSubject(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)

	ownerSubject, err := GetVolumeOwnerSubject("vol-ownerroute001")
	require.NoError(t, err)
	ownerSub, err := conn.Subscribe(ownerSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetVolumeResponse{
			Versioned: NewVersioned(), Volume: &Volume{ID: "vol-ownerroute001", Handle: "owner-handle"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ownerSub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	volume, err := provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-ownerroute001"})
	require.NoError(t, err)
	assert.Equal(t, "owner-handle", volume.Handle, "the owner subject's own responder must have answered")
}

// TestNATSProviderOwnerFirst_NoRespondersFallsBackToQueueSubject proves the
// one condition allowed to trigger a fallback: no responder at all on the
// owner subject. Only GetVolumeSubject (the queue-group subject) has a
// responder here, so success proves the fallback fired.
func TestNATSProviderOwnerFirst_NoRespondersFallsBackToQueueSubject(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)

	queueSub, err := conn.Subscribe(GetVolumeSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetVolumeResponse{
			Versioned: NewVersioned(), Volume: &Volume{ID: "vol-ownerfallback1", Handle: "queue-handle"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = queueSub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	volume, err := provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-ownerfallback1"})
	require.NoError(t, err)
	assert.Equal(t, "queue-handle", volume.Handle, "no owner responder must fall back to the queue-group subject")
}

// TestNATSProviderOwnerFirst_NonNoRespondersDoesNotFallBack is the dangerous
// case requestOwnerFirst exists to avoid: an owner subject that has a
// responder but never replies produces a timeout, not nats.ErrNoResponders,
// and must NOT fall back to the queue subject — a queue fallback here could
// run the same operation (e.g. a snapshot) a second time while the owner is
// still mid-flight. A responsive queue-subject responder is present so a
// wrongful fallback would succeed silently instead of failing loudly.
func TestNATSProviderOwnerFirst_NonNoRespondersDoesNotFallBack(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)

	ownerSubject, err := GetVolumeOwnerSubject("vol-nofallback001")
	require.NoError(t, err)
	ownerSub, err := conn.Subscribe(ownerSubject, func(msg *nats.Msg) {
		// Deliberately never responds, forcing the client to time out.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ownerSub.Unsubscribe() })

	var queueHits atomic.Int32
	queueSub, err := conn.Subscribe(GetVolumeSubject, func(msg *nats.Msg) {
		queueHits.Add(1)
		payload, marshalErr := json.Marshal(GetVolumeResponse{
			Versioned: NewVersioned(), Volume: &Volume{ID: "vol-nofallback001", Handle: "queue-handle"},
		})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = queueSub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, 200*time.Millisecond)
	_, err = provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-nofallback001"})
	require.Error(t, err, "the owner responder never replies, so this must time out")
	require.NotErrorIs(t, err, nats.ErrNoResponders, "a timeout must never be mistaken for no responders")
	assert.Equal(t, int32(0), queueHits.Load(), "a timeout must never fall back to the queue-group subject")
}

// respondWith subscribes subject with a canned reply, so a client-side guard
// can be driven against a response shape a healthy server never sends.
func respondWith(t *testing.T, conn *nats.Conn, subject string, response any) {
	t.Helper()
	payload, err := json.Marshal(response)
	require.NoError(t, err)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())
}

// TestNATSProviderRejectsEmptySuccessResponse covers the nil-payload guard on
// every method that returns a pointer. A reply carrying neither an error nor a
// body is a server bug, and without these guards each method would hand the
// caller a nil *Volume or *Snapshot alongside a nil error.
func TestNATSProviderRejectsEmptySuccessResponse(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	provider := NewNATSProvider(conn, time.Second)
	ctx := t.Context()
	empty := Versioned{SchemaVersion: SchemaVersion}

	t.Run("CreateVolume", func(t *testing.T) {
		respondWith(t, conn, CreateVolumeSubject, CreateVolumeResponse{Versioned: empty})
		_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-empty"})
		require.EqualError(t, err, "ebs.create returned no volume")
	})

	t.Run("GetVolume", func(t *testing.T) {
		respondWith(t, conn, GetVolumeSubject, GetVolumeResponse{Versioned: empty})
		_, err := provider.GetVolume(ctx, GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-empty"})
		require.EqualError(t, err, "ebs.describe returned no volume")
	})

	t.Run("ExpandVolume", func(t *testing.T) {
		respondWith(t, conn, ExpandVolumeSubject, ExpandVolumeResponse{Versioned: empty})
		_, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-empty"})
		require.EqualError(t, err, "ebs.expand returned no volume")
	})

	t.Run("CopySnapshot", func(t *testing.T) {
		respondWith(t, conn, CopySnapshotSubject, CopySnapshotResponse{Versioned: empty})
		_, err := provider.CopySnapshot(ctx, CopySnapshotRequest{
			Versioned: NewVersioned(), SourceSnapshotID: "snap-1", DestinationSnapshotID: "snap-2", VolumeID: "vol-empty",
		})
		require.EqualError(t, err, "ebs.snapshot.copy returned no snapshot")
	})

	t.Run("PublishVolume", func(t *testing.T) {
		subject, err := PublishSubject("node-empty")
		require.NoError(t, err)
		respondWith(t, conn, subject, PublishVolumeResponse{Versioned: empty})
		_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-empty", NodeID: "node-empty"})
		require.EqualError(t, err, subject+" returned no published volume")
	})

	t.Run("CreateSnapshot", func(t *testing.T) {
		subject, err := SnapshotSubject("vol-empty")
		require.NoError(t, err)
		respondWith(t, conn, subject, CreateSnapshotResponse{Versioned: empty})
		_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-empty", VolumeID: "vol-empty"})
		require.EqualError(t, err, subject+" returned no operation ID")
	})
}

// TestNATSProviderPropagatesProviderErrors covers responseError's populated
// branch on the enumeration and unpublish verbs, whose errors otherwise reach
// the caller as an empty page or a silent success.
func TestNATSProviderPropagatesProviderErrors(t *testing.T) {
	t.Run("ListSnapshots refusal", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		respondWith(t, conn, ListSnapshotsSubject, ListSnapshotsResponse{
			Versioned: NewVersioned(), Error: &ProviderError{Code: ErrorCodeUnsupportedCap, Message: "snapshot enumeration"},
		})
		_, err := NewNATSProvider(conn, time.Second).ListSnapshots(t.Context(), ListSnapshotsRequest{Versioned: NewVersioned()})
		require.ErrorIs(t, err, ErrUnsupportedCapability)
	})

	t.Run("ListSnapshots page", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		respondWith(t, conn, ListSnapshotsSubject, ListSnapshotsResponse{
			Versioned: NewVersioned(),
			Snapshots: []SnapshotRef{{ID: "snap-1", SourceVolumeID: "vol-1", Handle: "vb://snap-1"}},
			NextToken: "snap-1",
		})
		page, err := NewNATSProvider(conn, time.Second).ListSnapshots(t.Context(), ListSnapshotsRequest{Versioned: NewVersioned()})
		require.NoError(t, err)
		assert.Equal(t, []SnapshotRef{{ID: "snap-1", SourceVolumeID: "vol-1", Handle: "vb://snap-1"}}, page.Snapshots)
		assert.Equal(t, "snap-1", page.NextToken)
	})

	t.Run("UnpublishVolume", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		subject, err := UnpublishSubject("node-err")
		require.NoError(t, err)
		respondWith(t, conn, subject, UnpublishVolumeResponse{
			Versioned: NewVersioned(), Error: &ProviderError{Code: ErrorCodeNotFound, Message: "no such mount"},
		})
		err = NewNATSProvider(conn, time.Second).UnpublishVolume(t.Context(), UnpublishVolumeRequest{
			Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-err",
		})
		require.ErrorIs(t, err, ErrNotFound)
	})
}

// TestNATSProviderValidatesSubjectTokensBeforeTransport extends the check to
// every method that embeds a caller-supplied ID in a subject: an unsafe token
// must be refused before a request is published, or the client publishes to a
// wildcard subject and takes an unrelated node's reply as its own.
func TestNATSProviderValidatesSubjectTokensBeforeTransport(t *testing.T) {
	provider := NewNATSProvider(nil, time.Second)
	ctx := t.Context()

	t.Run("GetVolume", func(t *testing.T) {
		_, err := provider.GetVolume(ctx, GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol.*"})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
	t.Run("ExpandVolume", func(t *testing.T) {
		_, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol.*"})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
	t.Run("CopySnapshot", func(t *testing.T) {
		_, err := provider.CopySnapshot(ctx, CopySnapshotRequest{
			Versioned: NewVersioned(), SourceSnapshotID: "snap-1", DestinationSnapshotID: "snap-2", VolumeID: "vol.*",
		})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
	t.Run("CreateSnapshot volume", func(t *testing.T) {
		_, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: "vol.*"})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
	t.Run("CreateSnapshot snapshot", func(t *testing.T) {
		_, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap.*", VolumeID: "vol-1"})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
	t.Run("UnpublishVolume", func(t *testing.T) {
		err := provider.UnpublishVolume(ctx, UnpublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node.*"})
		require.ErrorIs(t, err, ErrInvalidArgument)
	})
}

// TestNATSProviderCreateSnapshotRejectsMismatchedCompletion covers the two
// identity checks on the asynchronous path. Both exist because the completion
// arrives out of band: a subject or operation ID the client did not ask for
// means it is reading someone else's snapshot result.
func TestNATSProviderCreateSnapshotRejectsMismatchedCompletion(t *testing.T) {
	t.Run("completion subject mismatch", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		subject, err := SnapshotSubject("vol-mismatch")
		require.NoError(t, err)
		respondWith(t, conn, subject, CreateSnapshotResponse{
			Versioned: NewVersioned(), OperationID: "op-1", CompletionSubject: "ebs.provider.v1.snapshot.response.someone-else",
		})

		provider := NewNATSProvider(conn, time.Second)
		_, err = provider.CreateSnapshot(t.Context(), CreateSnapshotRequest{
			Versioned: NewVersioned(), SnapshotID: "snap-mismatch", VolumeID: "vol-mismatch",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "snapshot completion subject mismatch")
	})

	t.Run("operation id mismatch", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		subject, err := SnapshotSubject("vol-opid")
		require.NoError(t, err)
		completionSubject, err := SnapshotCompletionSubject("snap-opid")
		require.NoError(t, err)

		sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
			accepted, marshalErr := json.Marshal(CreateSnapshotResponse{
				Versioned: NewVersioned(), OperationID: "op-expected", CompletionSubject: completionSubject,
			})
			require.NoError(t, marshalErr)
			require.NoError(t, msg.Respond(accepted))

			completed, marshalErr := json.Marshal(CreateSnapshotResponse{
				Versioned: NewVersioned(), OperationID: "op-other",
				Snapshot: &Snapshot{ID: "snap-opid", SourceVolumeID: "vol-opid", State: SnapshotStateCompleted},
			})
			require.NoError(t, marshalErr)
			require.NoError(t, conn.Publish(completionSubject, completed))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		require.NoError(t, conn.Flush())

		provider := NewNATSProvider(conn, time.Second)
		_, err = provider.CreateSnapshot(t.Context(), CreateSnapshotRequest{
			Versioned: NewVersioned(), SnapshotID: "snap-opid", VolumeID: "vol-opid",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "snapshot operation mismatch")
	})

	t.Run("already completed reply short-circuits the wait", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		subject, err := SnapshotSubject("vol-sync")
		require.NoError(t, err)
		respondWith(t, conn, subject, CreateSnapshotResponse{
			Versioned: NewVersioned(), OperationID: "op-sync",
			Snapshot: &Snapshot{ID: "snap-sync", SourceVolumeID: "vol-sync", State: SnapshotStateCompleted},
		})

		provider := NewNATSProvider(conn, time.Second)
		snapshot, err := provider.CreateSnapshot(t.Context(), CreateSnapshotRequest{
			Versioned: NewVersioned(), SnapshotID: "snap-sync", VolumeID: "vol-sync",
		})
		require.NoError(t, err, "a server answering synchronously must not be made to wait for a completion it will never publish")
		assert.Equal(t, "snap-sync", snapshot.ID)
	})
}

// TestResponseErrorRejectsZeroSchemaVersion covers responseError directly: a
// reply carrying the zero SchemaVersion (e.g. a server that forgot to set
// Versioned on an error path) must be rejected before its Error field is
// ever inspected, even when that field is nil.
func TestResponseErrorRejectsZeroSchemaVersion(t *testing.T) {
	err := responseError(0, nil)
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	err = responseError(0, &ProviderError{Code: ErrorCodeNotFound, Message: "ignored"})
	require.ErrorIs(t, err, ErrUnsupportedVersion, "the version guard must win over a populated error field")

	require.NoError(t, responseError(SchemaVersion, nil))

	err = responseError(SchemaVersion, &ProviderError{Code: ErrorCodeVolumeInUse, Message: "in use"})
	require.ErrorIs(t, err, ErrVolumeInUse)
}
