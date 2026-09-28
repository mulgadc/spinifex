package kvutil_test

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// scaleEnv gates every measurement in this file. They stand up a real embedded
// JetStream cluster and create thousands of streams in it, which takes minutes
// and gigabytes — far too much for the ordinary test run, and the numbers are
// only meaningful when nothing else is competing for the machine.
const scaleEnv = "SPX_KV_SCALE"

// requireScaleEnabled skips unless the operator asked for these explicitly.
func requireScaleEnabled(tb testing.TB) {
	tb.Helper()
	if os.Getenv(scaleEnv) == "" {
		tb.Skipf("set %s=1 to run the KV scale measurements", scaleEnv)
	}
}

// natsCluster is an embedded multi-server JetStream cluster, in this process.
//
// In-process is what makes the memory figure obtainable: the servers allocate
// on the same heap the test reads with runtime.ReadMemStats, so per-stream
// overhead is measurable without parsing anything out of an external process.
// It also means the figure includes the client and the test, so it is only
// ever read as a delta between two samples.
type natsCluster struct {
	servers []*server.Server
	opts    []*server.Options
	conn    *nats.Conn
	js      jetstream.JetStream
}

// startNATSCluster brings up n routed JetStream servers and connects to the
// first.
//
// Every server gets the whole route list, including its own, because clustered
// JetStream refuses to start on a server with no configured routes — so the
// ports cannot be discovered after start and have to be chosen up front.
func startNATSCluster(tb testing.TB, n int) *natsCluster {
	tb.Helper()
	if n < 1 {
		tb.Fatalf("startNATSCluster: need at least one server, got %d", n)
	}

	const clusterName = "kvscale"
	routePorts := reserveLocalPorts(tb, n)
	routes := make([]*url.URL, 0, n)
	for _, port := range routePorts {
		routeURL, err := url.Parse(fmt.Sprintf("nats://127.0.0.1:%d", port))
		require.NoError(tb, err)
		routes = append(routes, routeURL)
	}

	c := &natsCluster{
		servers: make([]*server.Server, n),
		opts:    make([]*server.Options, n),
	}
	for i := range n {
		// Held so a server can be rebuilt on the same store after a shutdown,
		// which is what the restart measurement needs.
		c.opts[i] = &server.Options{
			ServerName: fmt.Sprintf("kvscale-%d", i),
			Host:       "127.0.0.1",
			Port:       -1,
			JetStream:  true,
			StoreDir:   tb.TempDir(),
			NoLog:      true,
			NoSigs:     true,
			Cluster: server.ClusterOpts{
				Name: clusterName,
				Host: "127.0.0.1",
				Port: routePorts[i],
			},
			Routes: routes,
		}
		c.start(tb, i)
	}

	nc, err := nats.Connect(c.servers[0].ClientURL(),
		nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	require.NoError(tb, err, "connect to the embedded cluster")
	tb.Cleanup(nc.Close)
	c.conn = nc

	js, err := jetstream.New(nc)
	require.NoError(tb, err)
	c.js = js

	c.waitMetaLeader(tb, 30*time.Second)
	return c
}

// start builds and starts server i from its retained options, replacing any
// earlier instance. The store directory is the same one, so a server started
// this way rejoins with the state it had rather than as a fresh peer.
func (c *natsCluster) start(tb testing.TB, i int) {
	tb.Helper()
	srv, err := server.NewServer(c.opts[i])
	require.NoErrorf(tb, err, "build embedded server %d", i)
	go srv.Start()
	if !srv.ReadyForConnections(60 * time.Second) {
		tb.Fatalf("embedded server %d not ready for connections within 60s", i)
	}
	c.servers[i] = srv
	tb.Cleanup(srv.Shutdown)
}

// reserveLocalPorts asks the OS for n free loopback ports and releases them.
// They are held open together so the same port is not handed out twice, and
// released only once all n are known.
func reserveLocalPorts(tb testing.TB, n int) []int {
	tb.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(tb, err, "reserve a loopback port")
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range listeners {
		require.NoError(tb, ln.Close())
	}
	return ports
}

// waitMetaLeader blocks until the JetStream meta group has elected a leader.
// Creating a stream before that returns "no suitable peers", which would read
// as a placement failure rather than as the cluster still forming.
func (c *natsCluster) waitMetaLeader(tb testing.TB, within time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		leaders := 0
		for _, srv := range c.servers {
			if srv.JetStreamIsLeader() {
				leaders++
			}
		}
		if leaders == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	tb.Fatalf("no JetStream meta leader within %s", within)
}

// waitStreamsCurrent blocks until every server reports healthy, so a
// measurement taken after a restart is of a healed cluster rather than of one
// still catching up.
//
// Healthz is what makes this the right question: JetStreamIsCurrent covers the
// meta group alone, which catches up in milliseconds however many streams
// exist, while Healthz walks every assigned stream — and per-stream recovery is
// exactly what the bucket count multiplies.
func (c *natsCluster) waitStreamsCurrent(tb testing.TB, streams int, within time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(within)
	var last *server.HealthStatus
	for time.Now().Before(deadline) {
		ready := 0
		for _, srv := range c.servers {
			// The stream count is checked as well as health, because a server
			// that has just restarted has not yet been told what it hosts, and
			// reports healthy on the empty set it currently knows about.
			if jsz, err := srv.Jsz(nil); err != nil || jsz.Streams < streams {
				continue
			}
			status := srv.Healthz(nil)
			if status.StatusCode == http.StatusOK {
				ready++
				continue
			}
			last = status
		}
		if ready == len(c.servers) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	tb.Fatalf("servers did not all hold %d healthy streams within %s: %+v", streams, within, last)
}
