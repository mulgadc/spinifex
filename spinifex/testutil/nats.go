package testutil

import (
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
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
// and returns the server, a connected client, and a JetStream context.
// All resources are cleaned up via t.Cleanup.
func StartTestJetStream(t *testing.T) (*server.Server, *nats.Conn, nats.JetStreamContext) {
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

	js, err := nc.JetStream()
	require.NoError(t, err)
	return ns, nc, js
}

// vpcdStubTopics lists every synchronous vpcd request topic the unit-test
// stub auto-replies to. CreateSecurityGroup / Authorize / Revoke / Modify /
// CreateNetworkInterface all block on the 5s vpcd round-trip, so the stub
// has to cover each one or the tests time out.
var vpcdStubTopics = []string{
	"vpc.create-sg",
	"vpc.delete-sg",
	"vpc.update-sg",
	"vpc.update-port-sgs",
	"vpc.create-port",
}

// vpcdStubRegistry holds the per-conn response map shared between the
// subscribers and the OverrideVpcdStubResponse helper. Stored per-conn so
// concurrent tests with separate NATS conns don't trip on each other.
var (
	vpcdStubMu       sync.RWMutex
	vpcdStubRegistry = map[*nats.Conn]map[string][]byte{}
)

// StubVpcdSGResponder stands in for vpcd in unit tests by auto-replying
// success to the synchronous vpcd request topics. Use
// OverrideVpcdStubResponse to swap a topic's reply mid-test (negative-path
// tests) — that updates the same single subscriber rather than layering a
// second one, which would race the success replier.
func StubVpcdSGResponder(t *testing.T, nc *nats.Conn) {
	t.Helper()
	registerVpcdStub(t, nc, []byte(`{"success":true}`))
}

// StubVpcdSGFailingResponder is the negative-path counterpart to
// StubVpcdSGResponder: every stubbed topic replies success=false so tests
// can assert the handler propagates vpcd errors instead of swallowing them.
func StubVpcdSGFailingResponder(t *testing.T, nc *nats.Conn, errMsg string) {
	t.Helper()
	registerVpcdStub(t, nc, []byte(`{"success":false,"error":"`+errMsg+`"}`))
}

// OverrideVpcdStubResponse changes the stub's reply for one topic on the
// given conn. The change is in-place on the existing subscriber, so there
// is exactly one responder per topic — no race with a layered second
// subscriber. Use to make a single-topic negative-path assertion against
// an otherwise-success stub.
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
func SeedKV(t *testing.T, js nats.JetStreamContext, bucket string, entries map[string][]byte) nats.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)
	for k, v := range entries {
		_, err := kv.Put(k, v)
		require.NoError(t, err)
	}
	return kv
}
