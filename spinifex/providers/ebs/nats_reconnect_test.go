package ebsprovider

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A request on a connection that is down fails at once rather than buffering:
// one flushed on reconnect would run after its caller gave up on it.
func TestNATSProviderRequestRefusesWhileDisconnected(t *testing.T) {
	connections := map[string]func(t *testing.T) *nats.Conn{
		"Reconnecting": func(t *testing.T) *nats.Conn {
			ns := startNATS(t)
			nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(time.Hour))
			require.NoError(t, err)
			t.Cleanup(nc.Close)
			ns.Shutdown()
			require.Eventually(t, func() bool { return !nc.IsConnected() }, 5*time.Second, 5*time.Millisecond)
			require.False(t, nc.IsClosed(), "the connection must still be trying to reconnect")
			return nc
		},
		"Closed": func(t *testing.T) *nats.Conn {
			nc, err := nats.Connect(startNATS(t).ClientURL())
			require.NoError(t, err)
			nc.Close()
			return nc
		},
	}
	for name, connect := range connections {
		t.Run(name, func(t *testing.T) {
			provider := NewNATSProvider(connect(t), 5*time.Second)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			_, err := provider.UnpublishVolume(ctx, UnpublishVolumeRequest{
				Versioned: NewLatestVersioned(), VolumeID: "vol-1", NodeID: "node-down",
			})
			require.ErrorIs(t, err, nats.ErrConnectionClosed)
			assert.NotErrorIs(t, err, context.DeadlineExceeded, "the request must be refused, not left waiting")
		})
	}
}

func startNATS(t *testing.T) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(ns.Shutdown)
	return ns
}
