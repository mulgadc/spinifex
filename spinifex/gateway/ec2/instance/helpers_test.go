package gateway_ec2_instance

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// startTestNATSServer starts an embedded NATS server for testing.
func startTestNATSServer(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	return testutil.StartTestNATS(t)
}

// noopTerminateRetrySleep replaces the terminate NoResponders backoff with a
// no-op for the duration of the test, so retry paths do not burn real seconds.
func noopTerminateRetrySleep(t *testing.T) {
	t.Helper()
	prev := terminateRetrySleep
	terminateRetrySleep = func(time.Duration) {}
	t.Cleanup(func() { terminateRetrySleep = prev })
}

// subscribeAsNode replies on subject as nodeID, carrying the X-Node-ID header
// a real daemon reply would set, so identity-mode completeness can attribute
// the frame.
func subscribeAsNode(t *testing.T, nc *nats.Conn, subject, nodeID string, data []byte) {
	t.Helper()
	_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		reply := nats.NewMsg(msg.Reply)
		reply.Data = data
		if nodeID != "" {
			reply.Header.Set(utils.NodeIDHeader, nodeID)
		}
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
}
