package ebsprovider

import (
	"context"
	"encoding/json"
	"sync/atomic"
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
		assert.False(t, FenceNoticeRetryable(err))
	})

	t.Run("refused as unavailable", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned(), Error: &ProviderError{Code: ErrorCodeUnavailable, Message: "later"}})
		require.NoError(t, err)
		answerFenceNotices(t, conn, ack)

		_, err = NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		require.ErrorIs(t, err, ErrUnavailable)
		assert.True(t, FenceNoticeRetryable(err), "a daemon that cannot act yet may act on the next notice")
	})

	t.Run("undecodable answer", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		answerFenceNotices(t, conn, []byte("{"))

		_, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		var refused *ProviderError
		require.ErrorAs(t, err, &refused, "the daemon answered, so the notice was heard")
		assert.Equal(t, ErrorCodeInternal, refused.Code)
		assert.False(t, FenceNoticeRetryable(err))
	})

	t.Run("nobody listening", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		_, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		require.Error(t, err)
		var refused *ProviderError
		assert.NotErrorAs(t, err, &refused, "an unheard notice must read as unheard, or the sender stops retrying")
		assert.True(t, FenceNoticeRetryable(err))
	})

	t.Run("connection closed", func(t *testing.T) {
		_, conn := testutil.StartTestNATS(t)
		conn.Close()
		_, err := NotifyFenced(t.Context(), conn, fenceNoticeFor("vol-1"), time.Second)
		require.ErrorIs(t, err, nats.ErrConnectionClosed)
		assert.True(t, FenceNoticeRetryable(err), "a connection that comes back can still carry the notice")
	})
}

// TestNotifyFenced_AttributesTheNoticeToItsAccount holds the notice to the
// contract's request path: the daemon's span names the account it acts for.
func TestNotifyFenced_AttributesTheNoticeToItsAccount(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	subject, err := FenceNoticeSubject("node-a")
	require.NoError(t, err)
	accounts := make(chan string, 1)
	ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned()})
	require.NoError(t, err)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		accounts <- msg.Header.Get(AccountIDHeader)
		_ = msg.Respond(ack)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	_, err = NotifyFenced(WithAccountID(t.Context(), "123456789012"), conn, fenceNoticeFor("vol-1"), time.Second)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", <-accounts)
}

// TestNotifyFenced_RefusesAnUnaddressableNotice keeps a notice that can never
// be sent from reading as unheard, which a retrying sender would repeat forever.
func TestNotifyFenced_RefusesAnUnaddressableNotice(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	cases := map[string]struct {
		conn   *nats.Conn
		notice FenceNotice
	}{
		"no volume": {conn, FenceNotice{Versioned: NewVersioned(), NodeID: "node-a"}},
		"no node":   {conn, FenceNotice{Versioned: NewVersioned(), VolumeID: "vol-1"}},
		"bad node":  {conn, FenceNotice{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node.a"}},
		"no conn":   {nil, fenceNoticeFor("vol-1")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NotifyFenced(context.Background(), tc.conn, tc.notice, time.Second)
			require.ErrorIs(t, err, ErrInvalidArgument)
			assert.False(t, FenceNoticeRetryable(err), "a notice that can never be sent must not be retried")
		})
	}
}

// TestMemoryProviderRevokePublication_RetriesAnUnheardNotice keeps the memory
// provider to the fence's delivery rule: a notice nobody answered is sent
// again until one does, not dropped after the first try.
func TestMemoryProviderRevokePublication_RetriesAnUnheardNotice(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	ctx := t.Context()
	provider := NewMemoryProvider(Capabilities{})
	provider.fenceNoticeTimeout = 100 * time.Millisecond
	provider.fenceNoticeEvery = 10 * time.Millisecond
	provider.NotifyFencesOn(conn)
	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
	require.NoError(t, err)
	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-a"})
	require.NoError(t, err)

	subject, err := FenceNoticeSubject("node-a")
	require.NoError(t, err)
	ack, err := json.Marshal(FenceNoticeAck{Versioned: NewVersioned(), InstanceID: "i-stopped"})
	require.NoError(t, err)
	var heard atomic.Int32
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		if heard.Add(1) == 1 {
			return // the first notice is lost, as on a daemon still reconnecting
		}
		_ = msg.Respond(ack)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, conn.Flush())

	stopped, err := provider.RevokePublication(ctx, "vol-1", "node-b")
	require.NoError(t, err, "a lost first notice must be sent again")
	assert.Equal(t, "i-stopped", stopped)
	assert.Equal(t, int32(2), heard.Load())
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
