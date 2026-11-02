package kvstore_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startJetStreamCluster starts n routed JetStream servers and returns a handle
// connected to the first once a create can actually place n replicas.
func startJetStreamCluster(t *testing.T, n int) jetstream.JetStream {
	t.Helper()
	// Servers bind their own route ports, so none is ever free to be taken.
	// JetStream needs a configured route, so the first, which cannot name the
	// others yet, gets port 1, where nothing listens; the rest route to it.
	seed := []*url.URL{{Scheme: "nats", Host: "127.0.0.1:1"}}
	servers := make([]*server.Server, n)
	for i := range servers {
		ns, err := server.NewServer(&server.Options{
			ServerName: fmt.Sprintf("s%d", i+1), JetStream: true, StoreDir: t.TempDir(),
			Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
			Cluster: server.ClusterOpts{Name: "kvstore", Host: "127.0.0.1", Port: -1},
			Routes:  seed,
		})
		require.NoError(t, err)
		ns.Start()
		servers[i] = ns
		// Registered after its TempDir, so the server stops before that is removed.
		t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })
		require.True(t, ns.ReadyForConnections(10*time.Second), "server %d not ready", i+1)
		if i == 0 {
			seed = []*url.URL{{Scheme: "nats", Host: ns.ClusterAddr().String()}}
		}
	}

	// The meta leader places replicas only on peers it has stats for, which
	// can trail its election, so a create before then finds too few peers.
	require.Eventually(t, func() bool {
		for _, ns := range servers {
			if len(ns.JetStreamClusterPeers()) == n {
				return true
			}
		}
		return false
	}, 30*time.Second, 50*time.Millisecond, "meta leader never had stats for all %d peers", n)

	nc, err := nats.Connect("", nats.InProcessServer(servers[0]))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return js
}

// TestBucket_TTLBucketIsReplicatedAcrossTheCluster pins TTL and replica count
// together: a TTL bucket created at one replica loses its markers with one node,
// and the usage dedupe bucket is exactly that kind of bucket.
func TestBucket_TTLBucketIsReplicatedAcrossTheCluster(t *testing.T) {
	const nodes = 3
	js := startJetStreamCluster(t, nodes)
	clustersize.RedeclareForTest(t, nodes)

	bucket := kvstore.NewBucket(js, kvstore.Config{
		Name:    "kvstore-ttl-replicated",
		History: 1,
		TTL:     90 * time.Minute,
	})

	// Wait for a meta leader before the one open, so a retried open cannot find a
	// one-replica bucket from a timed-out create and raise it into a false pass.
	var lastErr error
	ready := assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		_, lastErr = js.AccountInfo(ctx)
		return lastErr == nil
	}, 30*time.Second, 50*time.Millisecond)
	require.True(t, ready, "JetStream cluster never elected a meta leader: %v", lastErr)

	kv, err := bucket.KV(t.Context())
	require.NoError(t, err)

	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 90*time.Minute, status.TTL(), "Config.TTL must reach the created bucket")
	bucketStatus, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "status is %T", status)
	assert.Equal(t, nodes, bucketStatus.StreamInfo().Config.Replicas,
		"a TTL bucket must be created at the cluster's replica count")
}
