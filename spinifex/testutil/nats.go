package testutil

import (
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// StartTestNATS starts an embedded NATS server (no JetStream) and returns
// the server and a connected client. Both are cleaned up via t.Cleanup.
func StartTestNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	opts := &server.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	return ns, nc
}

// StartTestJetStream starts an embedded NATS server with JetStream enabled
// and returns the server, a connected client, and a jetstream package handle.
// All resources are cleaned up via t.Cleanup.
func StartTestJetStream(t *testing.T) (*server.Server, *nats.Conn, jetstream.JetStream) {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return ns, nc, js
}

// NewJetStream returns a jetstream package handle for an existing test
// connection, for tests that hold a *nats.Conn without the tuple
// StartTestJetStream returns.
func NewJetStream(t *testing.T, nc *nats.Conn) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return js
}

// vpcdStubTopics lists every synchronous vpcd topic the stub auto-replies to.
// All block on a 5 s round-trip, so missing any one causes test timeouts.
var vpcdStubTopics = []string{
	"vpc.create-sg",
	"vpc.delete-sg",
	"vpc.update-sg",
	"vpc.update-port-sgs",
	"vpc.create-port",
}

// vpcdStubRegistry holds the per-conn response map. Per-conn so concurrent tests with separate conns don't interfere.
var (
	vpcdStubMu       sync.RWMutex
	vpcdStubRegistry = map[*nats.Conn]map[string][]byte{}
)

// StubVpcdSGResponder auto-replies success to all synchronous vpcd topics.
// Use OverrideVpcdStubResponse to swap a reply mid-test without adding a racing second subscriber.
func StubVpcdSGResponder(t *testing.T, nc *nats.Conn) {
	t.Helper()
	registerVpcdStub(t, nc, []byte(`{"success":true}`))
}

// StubVpcdSGFailingResponder is the negative-path counterpart: all topics reply success=false.
func StubVpcdSGFailingResponder(t *testing.T, nc *nats.Conn, errMsg string) {
	t.Helper()
	registerVpcdStub(t, nc, []byte(`{"success":false,"error":"`+errMsg+`"}`))
}

// OverrideVpcdStubResponse changes the stub's reply for one topic on the given conn.
// Updates the existing subscriber in-place — no second responder, no race.
func OverrideVpcdStubResponse(nc *nats.Conn, topic string, payload []byte) {
	vpcdStubMu.Lock()
	defer vpcdStubMu.Unlock()
	if vpcdStubRegistry[nc] == nil {
		vpcdStubRegistry[nc] = make(map[string][]byte, len(vpcdStubTopics))
	}
	vpcdStubRegistry[nc][topic] = payload
}

func registerVpcdStub(t *testing.T, nc *nats.Conn, defaultPayload []byte) {
	t.Helper()
	vpcdStubMu.Lock()
	if vpcdStubRegistry[nc] == nil {
		vpcdStubRegistry[nc] = make(map[string][]byte, len(vpcdStubTopics))
	}
	for _, topic := range vpcdStubTopics {
		vpcdStubRegistry[nc][topic] = defaultPayload
	}
	vpcdStubMu.Unlock()

	for _, topic := range vpcdStubTopics {
		sub, err := nc.Subscribe(topic, func(m *nats.Msg) {
			if m.Reply == "" {
				return
			}
			vpcdStubMu.RLock()
			resp := vpcdStubRegistry[nc][topic]
			vpcdStubMu.RUnlock()
			_ = m.Respond(resp)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	t.Cleanup(func() {
		vpcdStubMu.Lock()
		delete(vpcdStubRegistry, nc)
		vpcdStubMu.Unlock()
	})
}

// SeedKV creates a KV bucket and populates it with the given entries.
// Returns the KV handle for further use.
func SeedKV(t *testing.T, js jetstream.JetStream, bucket string, entries map[string][]byte) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)
	for k, v := range entries {
		_, err := kv.Put(t.Context(), k, v)
		require.NoError(t, err)
	}
	return kv
}
