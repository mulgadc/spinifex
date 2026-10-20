package kvstore_test

import (
	"context"
	"fmt"
	"net"
	"strings"
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
// connected to the first, so a create can actually place n replicas.
func startJetStreamCluster(t *testing.T, n int) jetstream.JetStream {
	t.Helper()
	ports := make([]int, n)
	routes := make([]string, n)
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ports[i] = l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		routes[i] = fmt.Sprintf("nats://127.0.0.1:%d", ports[i])
	}
	servers := make([]*server.Server, n)
	for i := range servers {
		ns, err := server.NewServer(&server.Options{
			ServerName: fmt.Sprintf("s%d", i+1), JetStream: true, StoreDir: t.TempDir(),
			Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
			Cluster: server.ClusterOpts{Name: "kvstore", Host: "127.0.0.1", Port: ports[i]},
			Routes:  server.RoutesFromStr(strings.Join(routes, ",")),
		})
		require.NoError(t, err)
		ns.Start()
		servers[i] = ns
	}
	t.Cleanup(func() {
		for _, ns := range servers {
			ns.Shutdown()
			ns.WaitForShutdown()
		}
	})

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

	// Before a meta leader is elected a create blocks for the client's default
	// timeout, so each attempt gets its own short deadline.
	var kv jetstream.KeyValue
	var err error
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		kv, err = bucket.KV(ctx)
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "open TTL bucket on the cluster: %v", err)

	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 90*time.Minute, status.TTL(), "Config.TTL must reach the created bucket")
	bucketStatus, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "status is %T", status)
	assert.Equal(t, nodes, bucketStatus.StreamInfo().Config.Replicas,
		"a TTL bucket must be created at the cluster's replica count")
}
