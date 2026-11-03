package ebsprovider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerFenceNotices replies to node-a's fence notices with reply.
func answerFenceNotices(t *testing.T, conn *nats.Conn, reply []byte) {
	t.Helper()
	subject, err := FenceNoticeSubject("node-a")
	require.NoError(t, err)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) { _ = msg.Respond(reply) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())
}

func fenceNoticeFor(volumeID string) FenceNotice {
	return FenceNotice{Versioned: NewVersioned(), VolumeID: volumeID, NodeID: "node-a", Winner: "node-b"}
}

// TestNotifyFenced_SeparatesHeardFromUnheard pins what a sender's retry turns
// on: an answer of any kind ends the notice, and only silence repeats it.
func TestNotifyFenced_SeparatesHeardFromUnheard(t *testing.T) {
	t.Run("acknowledged", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned(), InstanceID: "i-stopped"})
		require.NoError(t, err)
		answerFenceNotices(t, conn, ack)

		instanceID, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		require.NoError(t, err)
		assert.Equal(t, "i-stopped", instanceID)
	})

	t.Run("refused", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned(), Error: &ProviderError{Code: ErrorCodeInvalidArgument, Message: "no"}})
		require.NoError(t, err)
		answerFenceNotices(t, conn, ack)

		_, err = NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		var refused *ProviderError
		require.ErrorAs(t, err, &refused, "a refusal was heard, and repeating it would only be refused again")
		assert.ErrorIs(t, err, ErrInvalidArgument)
	})

	t.Run("undecodable answer", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		answerFenceNotices(t, conn, []byte("{"))

		_, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		var refused *ProviderError
		require.ErrorAs(t, err, &refused, "the daemon answered, so the notice was heard")
	})

	t.Run("nobody listening", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		_, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		require.Error(t, err)
		var refused *ProviderError
		assert.NotErrorAs(t, err, &refused, "an unheard notice must read as unheard, or the sender stops retrying")
	})
}

func TestNotifyFenced_RefusesAnUnaddressableNotice(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	_, err := NotifyFenced(context.Background(), conn, FenceNotice{Versioned: NewVersioned(), NodeID: "node-a"}, time.Second)
	require.ErrorIs(t, err, ErrInvalidArgument, "a notice naming no volume cannot stop a guest")

	_, err = NotifyFenced(context.Background(), conn, FenceNotice{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node.a"}, time.Second)
	require.ErrorIs(t, err, ErrInvalidArgument, "a node that is not one subject token has no fence-notice subject")

	_, err = NotifyFenced(context.Background(), nil, fenceNoticeFor("vol-1"), time.Second)
	require.ErrorIs(t, err, nats.ErrConnectionClosed)
}

func TestMemoryProviderRevokePublication(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	ctx := context.Background()
	provider := NewMemoryProvider(Capabilities{})
	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
	require.NoError(t, err)
	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-a"})
	require.NoError(t, err)

	_, err = provider.RevokePublication(ctx, "vol-1", "node-b")
	require.ErrorIs(t, err, ErrUnavailable, "with nowhere to announce it, a revocation would leave the guest unstopped")
	stillPublished, err := provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-b"})
	require.ErrorIs(t, err, ErrVolumeInUse, "a refused revocation must leave the publication in place")
	assert.Nil(t, stillPublished)

	ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned()})
	require.NoError(t, err)
	answerFenceNotices(t, conn, ack)
	provider.NotifyFencesOn(conn)

	_, err = provider.RevokePublication(ctx, "vol-1", "node-b")
	require.NoError(t, err)
	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-b"})
	require.NoError(t, err, "a revoked publication no longer holds the volume")

	_, err = provider.RevokePublication(ctx, "vol-never", "node-b")
	require.ErrorIs(t, err, ErrNotFound)
}
