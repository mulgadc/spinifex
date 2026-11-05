package instance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveTeardownOutstanding(t *testing.T, nc *nats.Conn, reply []byte) {
	t.Helper()
	_, err := nc.Subscribe(AccountTeardownOutstandingSubject, func(msg *nats.Msg) { _ = msg.Respond(reply) })
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
}

func TestAccountTeardownOutstanding_ReturnsTheServedInstances(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	reply, err := json.Marshal(ec2instance.TeardownOutstandingOutput{Instances: []ec2instance.OutstandingInstance{{InstanceID: "i-1"}}})
	require.NoError(t, err)
	serveTeardownOutstanding(t, nc, reply)

	got, err := AccountTeardownOutstanding(context.Background(), nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "i-1", got[0].InstanceID)
}

// A read that cannot be answered must never look like an empty list, because an
// empty list lets the storage cleanup believe no QEMU still holds a volume.
func TestAccountTeardownOutstanding_UnansweredReadIsAnError(t *testing.T) {
	t.Run("no responder", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)

		got, err := AccountTeardownOutstanding(context.Background(), nc, "123456789012")

		require.Error(t, err)
		assert.Nil(t, got)
		assert.ErrorIs(t, err, nats.ErrNoResponders)
	})
	t.Run("error reply", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveTeardownOutstanding(t, nc, awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal))

		got, err := AccountTeardownOutstanding(context.Background(), nc, "123456789012")

		require.Error(t, err)
		assert.Nil(t, got)
	})
	t.Run("unreadable reply", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveTeardownOutstanding(t, nc, []byte("not json"))

		got, err := AccountTeardownOutstanding(context.Background(), nc, "123456789012")

		require.Error(t, err)
		assert.Nil(t, got)
	})
}

// Not parallel: it shortens the package timeout.
func TestAccountTeardownOutstanding_TimeoutIsAnError(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := nc.Subscribe(AccountTeardownOutstandingSubject, func(*nats.Msg) {})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	previous := accountTeardownOutstandingTimeout
	accountTeardownOutstandingTimeout = 200 * time.Millisecond
	t.Cleanup(func() { accountTeardownOutstandingTimeout = previous })

	got, err := AccountTeardownOutstanding(context.Background(), nc, "123456789012")

	require.Error(t, err)
	assert.Nil(t, got)
}
