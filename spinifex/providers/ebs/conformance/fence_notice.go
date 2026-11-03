package conformance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// fenceNoticeWait bounds how long CaptureFenceNotice waits for the notice.
const fenceNoticeWait = 10 * time.Second

// CaptureFenceNotice answers nodeID's fence notices on conn as a daemon with no
// guest on the volume does, starts revoke, and returns the first notice heard.
// revoke runs on its own goroutine, since a sender may retry until answered.
func CaptureFenceNotice(t *testing.T, conn *nats.Conn, nodeID string, revoke func() error) ebsprovider.FenceNotice {
	t.Helper()
	subject, err := ebsprovider.FenceNoticeSubject(nodeID)
	require.NoError(t, err)

	heard := make(chan ebsprovider.FenceNotice, 1)
	sub, err := conn.Subscribe(subject, func(msg *nats.Msg) {
		var notice ebsprovider.FenceNotice
		ack := ebsprovider.FenceNoticeAck{Versioned: ebsprovider.NewVersioned()}
		if err := json.Unmarshal(msg.Data, &notice); err != nil {
			ack.Error = &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: err.Error()}
		} else {
			ack.Versioned = ebsprovider.ReplyVersioned(notice.SchemaVersion)
			select {
			case heard <- notice:
			default:
			}
		}
		if payload, err := json.Marshal(ack); err == nil {
			_ = msg.Respond(payload)
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, conn.Flush())

	revoked := make(chan error, 1)
	go func() { revoked <- revoke() }()

	timeout := time.After(fenceNoticeWait)
	for {
		select {
		case notice := <-heard:
			return notice
		case err := <-revoked:
			require.NoError(t, err, "revoking the export")
			revoked = nil
		case <-timeout:
			t.Fatalf("no fence notice on %s within %s of revoking the export; the daemon would leave its guest running on a revoked disk", subject, fenceNoticeWait)
		}
	}
}
