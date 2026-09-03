package gateway_ec2_instance

import (
	"os"
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

// TestMain neuters the terminate NoResponders backoff for the whole binary, so
// retry paths do not burn real seconds. Installed once rather than swapped per
// test: the seam is a package global and the tests that need it run in
// parallel, so their restores raced each other.
func TestMain(m *testing.M) {
	terminateRetrySleep = func(time.Duration) {}
	os.Exit(m.Run())
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
