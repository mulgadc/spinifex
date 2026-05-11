package daemon

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peerHealthFixture spins up an httptest TLS server per peer with toggleable
// /health responses, returning the cluster config the daemon-under-test should
// see plus a handle to flip each peer up or down at runtime.
type peerHealthFixture struct {
	peers []*peerStub
	cfg   *config.ClusterConfig
}

type peerStub struct {
	name    string
	srv     *httptest.Server
	healthy atomic.Bool
}

func (p *peerStub) setHealthy(v bool) { p.healthy.Store(v) }

func newPeerHealthFixture(t *testing.T, peerCount int) *peerHealthFixture {
	t.Helper()
	f := &peerHealthFixture{}
	nodes := map[string]config.Config{
		"node-self": {Host: "127.0.0.1"},
	}
	for i := range peerCount {
		stub := &peerStub{}
		stub.healthy.Store(true)
		stub.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if stub.healthy.Load() {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(stub.srv.Close)

		u, err := url.Parse(stub.srv.URL)
		require.NoError(t, err)
		host, port, err := net.SplitHostPort(u.Host)
		require.NoError(t, err)
		stub.name = "peer-" + string(rune('a'+i))
		nodes[stub.name] = config.Config{
			Host:        host,
			AdvertiseIP: host,
			Daemon:      config.DaemonConfig{Host: "0.0.0.0:" + port},
		}
		f.peers = append(f.peers, stub)
	}
	f.cfg = &config.ClusterConfig{Node: "node-self", Nodes: nodes}
	return f
}

// daemonForPeerHealth builds a minimal *Daemon usable by monitorPeerReachability:
// only ctx + clusterConfig + the two mode-signal atomics are required.
func daemonForPeerHealth(t *testing.T, cfg *config.ClusterConfig) (*Daemon, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{ctx: ctx, cancel: cancel, clusterConfig: cfg}
	t.Cleanup(cancel)
	return d, cancel
}

func TestPeerCount_SingleNode(t *testing.T) {
	cfg := &config.ClusterConfig{
		Node:  "only",
		Nodes: map[string]config.Config{"only": {}},
	}
	d := &Daemon{clusterConfig: cfg}
	assert.Equal(t, 0, d.peerCount())
}

func TestPeerCount_NilConfig(t *testing.T) {
	d := &Daemon{}
	assert.Equal(t, 0, d.peerCount())
}

func TestPeerCount_ThreeNodes(t *testing.T) {
	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {},
			"node-2": {},
			"node-3": {},
		},
	}
	d := &Daemon{clusterConfig: cfg}
	assert.Equal(t, 2, d.peerCount())
}

// TestMonitorPeerReachability_NoPeers exits immediately on a single-node
// config — the goroutine must not spin waiting on a ticker for a probe set
// that will always be empty.
func TestMonitorPeerReachability_NoPeers(t *testing.T) {
	cfg := &config.ClusterConfig{
		Node:  "only",
		Nodes: map[string]config.Config{"only": {}},
	}
	d, _ := daemonForPeerHealth(t, cfg)

	done := make(chan struct{})
	go func() {
		d.monitorPeerReachability()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorPeerReachability did not return on single-node config")
	}
}

// TestProbePeersOnce_FlipsOnHealthChange is a deterministic test of the probe
// logic — no ticker, no goroutine — so flaky-timing failures stay outside the
// scope of what we're asserting.
func TestProbePeersOnce_FlipsOnHealthChange(t *testing.T) {
	f := newPeerHealthFixture(t, 2)
	d, _ := daemonForPeerHealth(t, f.cfg)

	client := &http.Client{
		Timeout:   peerProbeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	peers := d.peerNodes()
	require.Len(t, peers, 2)

	d.probePeersOnce(client, peers)
	assert.True(t, d.peersReachable.Load(), "both peers healthy ⇒ reachable")

	for _, p := range f.peers {
		p.setHealthy(false)
	}
	d.probePeersOnce(client, peers)
	assert.False(t, d.peersReachable.Load(), "all peers unhealthy ⇒ unreachable")

	f.peers[1].setHealthy(true)
	d.probePeersOnce(client, peers)
	assert.True(t, d.peersReachable.Load(), "one peer healthy ⇒ reachable")
}

// TestProbePeerHealth_RejectsBadAddr asserts the helper fails closed on
// missing addressing rather than panicking or treating "" as reachable.
func TestProbePeerHealth_RejectsBadAddr(t *testing.T) {
	d, _ := daemonForPeerHealth(t, &config.ClusterConfig{})
	client := &http.Client{Timeout: 100 * time.Millisecond}
	assert.False(t, d.probePeerHealth(client, config.Config{}))
	assert.False(t, d.probePeerHealth(client, config.Config{Host: "1.2.3.4"})) // no Daemon.Host
	assert.False(t, d.probePeerHealth(client, config.Config{Host: "1.2.3.4", Daemon: config.DaemonConfig{Host: "not-a-host-port"}}))
}

// TestMonitorPeerReachability_EndToEnd runs the real goroutine against the
// fixture and asserts the false→true→false transitions arrive inside one
// probe cycle plus slack.
func TestMonitorPeerReachability_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ticker-driven peer probe test in short mode")
	}
	f := newPeerHealthFixture(t, 2)
	d, cancel := daemonForPeerHealth(t, f.cfg)

	for _, p := range f.peers {
		p.setHealthy(false)
	}

	go d.monitorPeerReachability()
	defer cancel()

	require.Eventually(t, func() bool { return !d.peersReachable.Load() },
		peerProbeInterval+2*peerProbeTimeout+time.Second, 50*time.Millisecond,
		"peersReachable should settle false when no peer responds")

	f.peers[0].setHealthy(true)
	require.Eventually(t, d.peersReachable.Load,
		peerProbeInterval+peerProbeTimeout+time.Second, 50*time.Millisecond,
		"peersReachable should flip true when any peer comes back")
}
