package jetstream_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message/conformance"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/stretchr/testify/require"
)

// r1Harness runs one case on its own single-node server. Reopen restarts
// the server over the same data, as a crash would. Advance is nil because
// the server's clock cannot be moved, so the backstop case skips; MaxAge
// is asserted on the stream config directly instead.
func r1Harness(t *testing.T) conformance.Harness {
	t.Helper()
	dir := t.TempDir()
	srv, js := startServer(t, dir)
	s, err := sqsjs.New(context.Background(), js, testConfig)
	require.NoError(t, err)
	return conformance.Harness{
		Store:         s,
		MessageShards: testConfig.MessageStreams,
		StateShards:   testConfig.StateBuckets,
		Reopen: func(t *testing.T) message.Store {
			srv.Shutdown()
			srv.WaitForShutdown()
			_, js := startServer(t, dir)
			s, err := sqsjs.New(context.Background(), js, testConfig)
			require.NoError(t, err)
			return s
		},
	}
}

func TestR1Conformance(t *testing.T) {
	conformance.Run(t, r1Harness)
}

// TestR1SharedInstanceConformance runs every case on one long-lived store,
// so each must pass beside other queues' data on the shared streams.
func TestR1SharedInstanceConformance(t *testing.T) {
	s, _ := newStore(t)
	conformance.Run(t, func(*testing.T) conformance.Harness {
		return conformance.Harness{Store: s, MessageShards: testConfig.MessageStreams, StateShards: testConfig.StateBuckets}
	})
}
