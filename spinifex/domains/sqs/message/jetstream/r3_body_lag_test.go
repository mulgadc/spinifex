package jetstream_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// delayProxy forwards TCP to target, holding each chunk for the current
// delay in both directions while preserving order. A connection accepted
// before setTarget waits for it.
type delayProxy struct {
	l      net.Listener
	target string
	ready  chan struct{}
	delay  atomic.Int64
}

func newDelayProxy(t *testing.T) *delayProxy {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &delayProxy{l: l, ready: make(chan struct{})}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go p.forward(c)
		}
	}()
	return p
}

// setTarget is called once, with the address the target server bound.
func (p *delayProxy) setTarget(addr string) {
	p.target = addr
	close(p.ready)
}

func (p *delayProxy) forward(c net.Conn) {
	<-p.ready
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	go p.pipe(c, u)
	go p.pipe(u, c)
}

type delayedChunk struct {
	due time.Time
	b   []byte
}

func (p *delayProxy) pipe(src, dst net.Conn) {
	q := make(chan delayedChunk, 1<<16)
	go func() {
		defer func() { _ = dst.Close() }()
		for ch := range q {
			time.Sleep(time.Until(ch.due))
			if _, err := dst.Write(ch.b); err != nil {
				_ = src.Close()
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			q <- delayedChunk{due: time.Now().Add(time.Duration(p.delay.Load())), b: append([]byte(nil), buf[:n]...)}
		}
		if err != nil {
			close(q)
			return
		}
	}
}

// lagCluster is a three-node cluster whose routes all pass through delay
// proxies; proxy[i][j] carries the routes server i dials to server j.
type lagCluster struct {
	servers [3]*server.Server
	proxy   [3][3]*delayProxy
}

func startLagCluster(t *testing.T) *lagCluster {
	clustersize.RedeclareForTest(t, 3)
	c := &lagCluster{}
	for i := range 3 {
		for j := range 3 {
			if i != j {
				c.proxy[i][j] = newDelayProxy(t)
			}
		}
	}
	for i := range 3 {
		var routes []*url.URL
		for j := range 3 {
			if i != j {
				routes = append(routes, &url.URL{Scheme: "nats", Host: c.proxy[i][j].l.Addr().String()})
			}
		}
		srv, err := server.NewServer(&server.Options{
			ServerName: fmt.Sprintf("lag-%d", i), Host: "127.0.0.1", Port: server.RANDOM_PORT,
			JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
			// An unreachable advertised address stops gossip-learned routes that
			// would bypass the proxies; the configured routes are a full mesh.
			Cluster: server.ClusterOpts{Name: "lag", Host: "127.0.0.1", Port: server.RANDOM_PORT, PoolSize: -1, NoAdvertise: true, Advertise: "127.0.0.1:1"},
			Routes:  routes,
		})
		require.NoError(t, err)
		go srv.Start()
		c.servers[i] = srv
		t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	}
	for i, srv := range c.servers {
		require.True(t, srv.ReadyForConnections(10*time.Second), "node %d", i)
	}
	// Each server bound its own route port, so none was free to be taken.
	for i := range 3 {
		for j := range 3 {
			if i != j {
				c.proxy[i][j].setTarget(c.servers[j].ClusterAddr().String())
			}
		}
	}
	return c
}

// setLag delays the routes between servers a and b by d.
func (c *lagCluster) setLag(a, b int, d time.Duration) {
	c.proxy[a][b].delay.Store(int64(d))
	c.proxy[b][a].delay.Store(int64(d))
}

// localMsgs is the message count of stream in srv's own replica.
func localMsgs(t *testing.T, srv *server.Server, stream string) uint64 {
	info, err := srv.Jsz(&server.JSzOptions{Accounts: true, Streams: true})
	require.NoError(t, err)
	for _, a := range info.AccountDetails {
		for _, sd := range a.Streams {
			if sd.Name == stream {
				return sd.State.Msgs
			}
		}
	}
	return 0
}

// TestR3BodyEnumerationAfterDeleteWithLaggingFollower deletes a queue whose
// bodies follow a surviving queue's, while one follower of the message
// stream is held behind, then lists and scans the shard at once. A round
// counts only when the follower measurably still holds the purged bodies.
func TestR3BodyEnumerationAfterDeleteWithLaggingFollower(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a follower behind for several seconds")
	}
	const rounds, batch, lag = 6, 20, 2 * time.Second
	c := startLagCluster(t)
	ctx := context.Background()
	stream := sqsjs.StreamName(0)

	var meta int
	require.Eventually(t, func() bool {
		for i, srv := range c.servers {
			if srv.JetStreamIsLeader() {
				meta = i
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond)
	nc, err := nats.Connect(c.servers[meta].ClientURL(), nats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	var s *sqsjs.Store
	require.Eventually(t, func() bool {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		s, err = sqsjs.New(cctx, js, testConfig)
		return err == nil
	}, 60*time.Second, 500*time.Millisecond)

	lead := -1
	require.Eventually(t, func() bool {
		for i, srv := range c.servers {
			if srv.JetStreamIsStreamLeader(server.DEFAULT_GLOBAL_ACCOUNT, stream) {
				lead = i
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond)
	// Hold back a follower that is not the meta leader, which serves the client.
	far := 3 - meta - lead
	if meta == lead {
		far = (lead + 1) % 3
	}

	// The delete also purges the state bucket, which must not wait on far.
	kv := "KV_" + sqsjs.BucketName(0)
	for c.servers[far].JetStreamIsStreamLeader(server.DEFAULT_GLOBAL_ACCOUNT, kv) {
		require.NoError(t, c.servers[far].JetStreamStepdownStream(server.DEFAULT_GLOBAL_ACCOUNT, kv))
		require.Eventually(t, func() bool {
			for _, srv := range c.servers {
				if srv.JetStreamIsStreamLeader(server.DEFAULT_GLOBAL_ACCOUNT, kv) {
					return true
				}
			}
			return false
		}, 30*time.Second, 100*time.Millisecond)
	}

	held := 0
	for r := range rounds {
		keep := ns(0, 0, fmt.Sprintf("keep-%d", r))
		gone := ns(0, 0, fmt.Sprintf("gone-%d", r))
		appendTo(t, s, keep, "k")
		for range batch {
			appendTo(t, s, gone, "g")
		}
		require.Eventually(t, func() bool {
			return localMsgs(t, c.servers[far], stream) == localMsgs(t, c.servers[lead], stream)
		}, 30*time.Second, 50*time.Millisecond, "round %d: follower never caught up", r)

		// Only the leader's replication to far is held; far stays in touch
		// with the meta leader, so it remains eligible to host a consumer.
		c.setLag(lead, far, lag)
		require.NoError(t, s.DeleteNamespace(ctx, gone))
		if localMsgs(t, c.servers[far], stream) > localMsgs(t, c.servers[lead], stream) {
			held++
			got, err := s.Namespaces(ctx, 0)
			require.NoError(t, err)
			for _, n := range got {
				require.NotEqual(t, gone.Queue, n.Namespace.Queue, "round %d: deleted queue listed with usage %+v", r, n.Usage)
			}
			page := message.ScanPage{}
			for !page.Done {
				page, err = s.Scan(ctx, 0, page.Next, 100)
				require.NoError(t, err)
				for _, ch := range page.Changes {
					require.NotEqual(t, gone.Queue, ch.Namespace.Queue, "round %d: scan returned deleted body at %d", r, ch.Position)
				}
			}
		}
		c.setLag(lead, far, 0)
	}
	t.Logf("%d of %d rounds ran with the follower holding purged bodies", held, rounds)
	if held == 0 {
		t.Skip("the follower was never held behind, so no round tested the lag")
	}
}
