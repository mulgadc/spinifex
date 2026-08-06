package ebsprovider

import (
	"encoding/json"
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
