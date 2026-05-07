package testutil

import (
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

// StubVpcdSGResponder stands in for vpcd in unit tests by auto-replying
// success to the synchronous SG/port-SG NATS request topics
// (vpc.create-sg, vpc.delete-sg, vpc.update-sg, vpc.update-port-sgs).
// Without it every CreateSecurityGroup / Authorize / Revoke / Modify call
// would block on the 5s vpcd round-trip enforced by Phase 7.
func StubVpcdSGResponder(t *testing.T, nc *nats.Conn) {
	t.Helper()
	topics := []string{
		"vpc.create-sg",
		"vpc.delete-sg",
		"vpc.update-sg",
		"vpc.update-port-sgs",
	}
	for _, topic := range topics {
		sub, err := nc.Subscribe(topic, func(m *nats.Msg) {
			if m.Reply != "" {
				_ = m.Respond([]byte(`{"success":true}`))
			}
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
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
