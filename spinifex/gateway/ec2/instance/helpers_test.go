package gateway_ec2_instance

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestNATSServer starts an embedded NATS server for testing
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
