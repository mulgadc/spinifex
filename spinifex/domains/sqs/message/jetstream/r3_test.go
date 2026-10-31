package jetstream_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message/conformance"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cluster is a real three-node JetStream cluster in this process: separate
// stores, routes and Raft groups, restartable over the same data and ports.
type cluster struct {
	t       *testing.T
	dirs    [3]string
	client  [3]int
	route   [3]int
	logDir  string
	servers [3]*server.Server
	js      jsapi.JetStream
}

// routePorts reserves three distinct ports at once. Route ports must stay
// fixed across restarts; client ports are chosen by each server's first start.
func routePorts(t *testing.T) [3]int {
	t.Helper()
	var ports [3]int
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer l.Close()
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	return ports
}

func startCluster(t *testing.T) *cluster {
	t.Helper()
	return startClusterLogged(t, "")
}

// startClusterLogged writes each node's server log under logDir, when set.
func startClusterLogged(t *testing.T, logDir string) *cluster {
	t.Helper()
	c := &cluster{t: t, logDir: logDir}
	for i := range 3 {
		c.dirs[i] = t.TempDir()
	}
	c.route = routePorts(t)
	clustersize.RedeclareForTest(t, 3)
	c.start()
	t.Cleanup(c.stop)
	return c
}

func (c *cluster) start() {
	t := c.t
	routes := make([]*url.URL, 3)
	for i := range 3 {
		routes[i] = &url.URL{Scheme: "nats", Host: fmt.Sprintf("127.0.0.1:%d", c.route[i])}
	}
	for i := range 3 {
		port := c.client[i]
		if port == 0 {
			port = server.RANDOM_PORT
		}
		opts := &server.Options{
			ServerName: fmt.Sprintf("sqs-%d", i), Host: "127.0.0.1", Port: port,
			JetStream: true, StoreDir: c.dirs[i], NoLog: true, NoSigs: true,
			Cluster: server.ClusterOpts{Name: "sqs-r3", Host: "127.0.0.1", Port: c.route[i]},
			Routes:  routes,
		}
		if c.logDir != "" {
			opts.NoLog, opts.LogFile = false, filepath.Join(c.logDir, fmt.Sprintf("sqs-%d.log", i))
			opts.Debug = os.Getenv("SQS_R3_SERVER_DEBUG") != ""
		}
		srv, err := server.NewServer(opts)
		require.NoError(t, err)
		if c.logDir != "" {
			srv.ConfigureLogger()
		}
		go srv.Start()
		c.servers[i] = srv
	}
	urls := make([]string, 3)
	for i, srv := range c.servers {
		require.True(t, srv.ReadyForConnections(10*time.Second), "node %d not ready", i)
		// Keep the client port across restarts so open connections reconnect.
		c.client[i] = srv.Addr().(*net.TCPAddr).Port
		urls[i] = srv.ClientURL()
	}
	nc, err := nats.Connect(strings.Join(urls, ","), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := js.AccountInfo(ctx)
		return err == nil
	}, 30*time.Second, 100*time.Millisecond, "JetStream meta leader not elected")
	// After a restart the meta leader returns before each stream's Raft
	// group has elected its own leader, which every stream call needs.
	require.Eventually(t, func() bool { return allStreamsLed(js) }, 60*time.Second, 200*time.Millisecond, "stream leaders not elected")
	c.js = js
}

func allStreamsLed(js jsapi.JetStream) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lister := js.ListStreams(ctx)
	for info := range lister.Info() {
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return false
		}
	}
	return lister.Err() == nil
}

func (c *cluster) stop() {
	for _, srv := range c.servers {
		if srv != nil {
			srv.Shutdown()
			srv.WaitForShutdown()
		}
	}
}

func (c *cluster) store(t *testing.T) *sqsjs.Store {
	t.Helper()
	// R3 placement fails until the meta leader has every peer's config and
	// stats, which can trail its election on a loaded host; New is idempotent.
	var s *sqsjs.Store
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s, err = sqsjs.New(ctx, c.js, testConfig)
		cancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, err)
	return s
}

// TestR3Conformance runs the suite on a three-node cluster. Every case shares
// the cluster, using fresh queue IDs; Reopen stops all three nodes, as a
// Region-wide crash would, and restarts them over the same data.
func TestR3Conformance(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startCluster(t)
	conformance.Run(t, func(t *testing.T) conformance.Harness {
		return conformance.Harness{
			Store:         c.store(t),
			MessageShards: testConfig.MessageStreams,
			StateShards:   testConfig.StateBuckets,
			Reopen: func(t *testing.T) message.Store {
				c.stop()
				c.start()
				return c.store(t)
			},
		}
	})
}

// TestR3ProvisionsThreeReplicas: on three nodes every stream and state
// bucket is R3, never the R5 a larger cluster rule would give.
func TestR3ProvisionsThreeReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startCluster(t)
	c.store(t)
	ctx := context.Background()
	for i := range testConfig.MessageStreams {
		stream, err := c.js.Stream(ctx, sqsjs.StreamName(message.Shard(i)))
		require.NoError(t, err)
		assert.Equal(t, 3, stream.CachedInfo().Config.Replicas, "stream %d", i)
	}
	for i := range testConfig.StateBuckets {
		stream, err := c.js.Stream(ctx, "KV_"+sqsjs.BucketName(message.Shard(i)))
		require.NoError(t, err)
		assert.Equal(t, 3, stream.CachedInfo().Config.Replicas, "state bucket %d", i)
	}
}
