package jetstream_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
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
// stores, routes and Raft groups, restartable over the same data.
type cluster struct {
	t       *testing.T
	dirs    [3]string
	logDir  string
	mu      sync.Mutex // guards servers against a reconnecting client
	servers [3]*server.Server
	js      jsapi.JetStream
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
	c.start()
	t.Cleanup(c.stop)
	return c
}

// start binds every listener on a port the server picks itself, so no port
// is ever free to be taken. Nodes 1 and 2 route to node 0 and learn each
// other from it.
func (c *cluster) start() {
	t := c.t
	// JetStream needs a configured route, so node 0, which cannot name the
	// others yet, gets port 1, where nothing listens.
	seed := []*url.URL{{Scheme: "nats", Host: "127.0.0.1:1"}}
	for i := range 3 {
		opts := &server.Options{
			ServerName: fmt.Sprintf("sqs-%d", i), Host: "127.0.0.1", Port: server.RANDOM_PORT,
			JetStream: true, StoreDir: c.dirs[i], NoLog: true, NoSigs: true,
			Cluster: server.ClusterOpts{Name: "sqs-r3", Host: "127.0.0.1", Port: server.RANDOM_PORT},
			Routes:  seed,
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
		require.True(t, srv.ReadyForConnections(10*time.Second), "node %d not ready", i)
		if i == 0 {
			seed = []*url.URL{{Scheme: "nats", Host: srv.ClusterAddr().String()}}
		}
		c.mu.Lock()
		c.servers[i] = srv
		c.mu.Unlock()
	}
	nc, err := nats.Connect("", nats.InProcessServer(c), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
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

// InProcessConn connects a client to a running node. A client opened before
// a restart reconnects through it to the restarted nodes.
func (c *cluster) InProcessConn() (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, srv := range c.servers {
		if srv != nil && srv.Running() {
			return srv.InProcessConn()
		}
	}
	return nil, errors.New("no cluster node is running")
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

// r3Env enables TestR3; CI's race tier sets it, local preflight does not.
const r3Env = "SPINIFEX_SQS_R3"

// TestR3 owns the process-wide topology until all parallel scenarios and
// their server cleanups finish. Limit concurrency to three isolated clusters.
func TestR3(t *testing.T) { //nolint:tparallel // The parent must exclude R1 tests while it owns the global topology.
	if os.Getenv(r3Env) == "" {
		t.Skipf("three-node restart scenarios take ~40s; set %s=1 to run them (make test-race does)", r3Env)
	}
	clustersize.RedeclareForTest(t, 3)
	slots := make(chan struct{}, 3)
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"Conformance", testR3Conformance},
		{"FeedOpenedAfterRestart", testR3FeedOpenedAfterRestart},
		{"FeedOpenAcrossRestart", testR3FeedOpenAcrossRestart},
		{"BodyEnumerationAfterDeleteWithLaggingFollower", testR3BodyEnumerationAfterDeleteWithLaggingFollower},
		{"GetStateAfterDeleteFromFollower", testR3GetStateAfterDeleteFromFollower},
		{"StateEnumerationAfterDeleteFromFollower", testR3StateEnumerationAfterDeleteFromFollower},
		{"ProvisionsThreeReplicas", testR3ProvisionsThreeReplicas},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			t.Cleanup(func() { <-slots })
			tc.run(t)
		})
	}
}

// testR3Conformance runs the suite on a three-node cluster. Every case shares
// the cluster, using fresh queue IDs; Reopen stops all three nodes, as a
// Region-wide crash would, and restarts them over the same data.
func testR3Conformance(t *testing.T) {
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
func testR3ProvisionsThreeReplicas(t *testing.T) {
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
