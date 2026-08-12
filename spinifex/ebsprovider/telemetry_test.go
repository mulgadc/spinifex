package ebsprovider

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

func TestSubjectVerb_StripsIDsFromEveryDynamicSubject(t *testing.T) {
	const volumeID = "vol-0123456789abcdef0"
	const nodeID = "casuarina"

	ownerDescribe, err := GetVolumeOwnerSubject(volumeID)
	require.NoError(t, err)
	ownerExpand, err := ExpandVolumeOwnerSubject(volumeID)
	require.NoError(t, err)
	ownerSnapshotCreate, err := SnapshotCreateOwnerSubject(volumeID)
	require.NoError(t, err)
	ownerSnapshotCopy, err := SnapshotCopyOwnerSubject(volumeID)
	require.NoError(t, err)
	snapshotCreate, err := SnapshotSubject(volumeID)
	require.NoError(t, err)
	publish, err := PublishSubject(nodeID)
	require.NoError(t, err)
	unpublish, err := UnpublishSubject(nodeID)
	require.NoError(t, err)

	cases := map[string]string{
		CapabilitiesSubject:   verbCapabilities,
		CreateVolumeSubject:   verbVolumeCreate,
		GetVolumeSubject:      verbVolumeDescribe,
		ListVolumesSubject:    verbVolumeList,
		ExpandVolumeSubject:   verbVolumeExpand,
		DeleteVolumeSubject:   verbVolumeDelete,
		DeleteSnapshotSubject: verbSnapshotDelete,
		CopySnapshotSubject:   verbSnapshotCopy,
		ownerDescribe:         verbVolumeDescribe,
		ownerExpand:           verbVolumeExpand,
		ownerSnapshotCreate:   verbSnapshotCreate,
		ownerSnapshotCopy:     verbSnapshotCopy,
		snapshotCreate:        verbSnapshotCreate,
		publish:               verbVolumePublish,
		unpublish:             verbVolumeUnpublish,
	}

	for subject, want := range cases {
		verb := SubjectVerb(subject)
		assert.Equal(t, want, verb, "subject %q", subject)
		assert.NotContains(t, verb, volumeID, "verb for %q leaks the volume ID", subject)
		assert.NotContains(t, verb, nodeID, "verb for %q leaks the node ID", subject)
	}
}

func TestSubjectVerb_UnrecognisedSubjectCollapsesToUnknown(t *testing.T) {
	for _, subject := range []string{
		"",
		"ebs.mount",
		"ebs.provider.v1.volume.invent",
		"ebs.provider.v1.owner.vol-1.invent",
		"ebs.provider.v1.node.rename",
		"ebs.provider.v2.volume.create",
	} {
		assert.Equal(t, VerbUnknown, SubjectVerb(subject), "subject %q", subject)
	}
}

// TestClientSpan_NamesTheVerbNotTheSubject pins the property that makes these
// spans aggregatable: a per-volume subject must not become a span name of its
// own, so the volume ID belongs in an attribute.
func TestClientSpan_NamesTheVerbNotTheSubject(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, conn := testutil.StartTestNATS(t)

	const volumeID = "vol-0123456789abcdef0"
	ownerSubject, err := GetVolumeOwnerSubject(volumeID)
	require.NoError(t, err)
	sub, err := conn.Subscribe(ownerSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetVolumeResponse{Versioned: NewVersioned(), Volume: &Volume{ID: volumeID}})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	_, err = provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: volumeID})
	require.NoError(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "ebs.volume.describe", spans[0].Name())
	assert.NotContains(t, spans[0].Name(), volumeID)
	assert.Equal(t, volumeID, testutil.SpanAttribute(t, spans[0], attrVolumeID).AsString())
	assert.Equal(t, ownerSubject, testutil.SpanAttribute(t, spans[0], attrSubject).AsString())
	assert.True(t, testutil.SpanAttribute(t, spans[0], attrOwnerRouted).AsBool())
}

// TestClientSpan_PutsTraceparentOnTheWire is the half of propagation this
// package owns. Without it the serving provider roots a trace of its own and
// the two halves of an operation can never be joined.
func TestClientSpan_PutsTraceparentOnTheWire(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, conn := testutil.StartTestNATS(t)

	received := make(chan string, 1)
	sub, err := conn.Subscribe(ListVolumesSubject, func(msg *nats.Msg) {
		received <- msg.Header.Get("traceparent")
		payload, marshalErr := json.Marshal(ListVolumesResponse{Versioned: NewVersioned()})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	_, err = provider.ListVolumes(t.Context(), ListVolumesRequest{Versioned: NewVersioned()})
	require.NoError(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	select {
	case traceparent := <-received:
		require.NotEmpty(t, traceparent, "no traceparent header reached the provider")
		assert.Contains(t, traceparent, spans[0].SpanContext().TraceID().String())
		assert.Contains(t, traceparent, spans[0].SpanContext().SpanID().String())
	case <-time.After(time.Second):
		t.Fatal("provider never received the request")
	}
}

// TestClientSpan_RecordsTheWireErrorCode covers the case a transport-only span
// would report as success: the round trip completed, but the operation failed.
func TestClientSpan_RecordsTheWireErrorCode(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, conn := testutil.StartTestNATS(t)

	sub, err := conn.Subscribe(DeleteVolumeSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(DeleteVolumeResponse{
			Versioned: NewVersioned(),
			Error:     &ProviderError{Code: ErrorCodeVolumeInUse, Message: "volume vol-1 is published"},
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

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Equal(t, string(ErrorCodeVolumeInUse), testutil.SpanAttribute(t, spans[0], attrErrorCode).AsString())
}

// TestClientSpan_OwnerFallbackRecordsBothLegs keeps the wasted owner attempt
// visible. A fallback that showed only the leg that answered would hide the
// cost of asking a node that holds nothing.
func TestClientSpan_OwnerFallbackRecordsBothLegs(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, conn := testutil.StartTestNATS(t)

	const volumeID = "vol-0123456789abcdef0"
	sub, err := conn.Subscribe(GetVolumeSubject, func(msg *nats.Msg) {
		payload, marshalErr := json.Marshal(GetVolumeResponse{Versioned: NewVersioned(), Volume: &Volume{ID: volumeID}})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	provider := NewNATSProvider(conn, time.Second)
	_, err = provider.GetVolume(t.Context(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: volumeID})
	require.NoError(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 2, "owner attempt and queue-group fallback should each be a span")
	assert.Equal(t, "ebs.volume.describe", spans[0].Name())
	assert.True(t, testutil.SpanAttribute(t, spans[0], attrOwnerRouted).AsBool())
	assert.Equal(t, codes.Error, spans[0].Status().Code, "the unanswered owner attempt is a failure")

	assert.Equal(t, "ebs.volume.describe", spans[1].Name())
	assert.Equal(t, GetVolumeSubject, testutil.SpanAttribute(t, spans[1], attrSubject).AsString())
	assert.Equal(t, codes.Unset, spans[1].Status().Code)
}
