package ebsprovider

import (
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startNATSOnPort(t *testing.T, port int) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(ns.Shutdown)
	return ns
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// A request made while the connection is reconnecting is buffered and answered
// once it is back, as a plain nats request is, rather than refused.
func TestNATSProviderRequestBuffersThroughReconnect(t *testing.T) {
	port := freeTCPPort(t)
	ns := startNATSOnPort(t, port)
	connect := func(wait time.Duration) *nats.Conn {
		nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(wait), nats.ReconnectJitter(0, 0))
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		return nc
	}
	// The responder reconnects well before the client, so the buffered request
	// finds its subscription restored rather than no responder.
	client, server := connect(time.Second), connect(10*time.Millisecond)

	subject, err := UnpublishSubject("node-reconnect")
	require.NoError(t, err)
	respondWith(t, server, subject, UnpublishVolumeResponse{Versioned: NewVersioned()})

	ns.Shutdown()
	require.Eventually(t, func() bool { return client.IsReconnecting() }, 3*time.Second, 10*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := NewNATSProvider(client, 5*time.Second).UnpublishVolume(t.Context(), UnpublishVolumeRequest{
			Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-reconnect",
		})
		done <- err
	}()
	startNATSOnPort(t, port)
	require.Eventually(t, server.IsConnected, 3*time.Second, 5*time.Millisecond)
	require.NoError(t, server.Flush())
	assert.True(t, client.IsReconnecting(), "the request must still be waiting on the reconnect")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("request made while reconnecting was never answered")
	}
}

// A closed connection is not coming back, so a request fails at once.
func TestNATSProviderRequestFailsOnClosedConnection(t *testing.T) {
	ns := startNATSOnPort(t, -1)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	_, err = NewNATSProvider(nc, 5*time.Second).UnpublishVolume(t.Context(), UnpublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-1", NodeID: "node-closed",
	})
	require.ErrorIs(t, err, nats.ErrConnectionClosed)
	assert.NotErrorIs(t, err, nats.ErrTimeout)
}
