package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peerHealthFixture spins up an httptest TLS server per peer with toggleable
// /health responses, returning the cluster config the daemon-under-test should
// see plus a handle to flip each peer up or down at runtime.
type peerHealthFixture struct {
	peers  []*peerStub
	cfg    *config.ClusterConfig
	caPath string
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
		f.caPath = writeServerCA(t, stub.srv)
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

// writeServerCA writes srv's self-signed cert as a PEM CA file, standing in for
// the cluster CA. Every httptest TLS server shares that one cert.
func writeServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return writePEMCert(t, srv.Certificate().Raw)
}

// writeUntrustedCA writes a freshly generated self-signed CA that signed none
// of the httptest servers' certs.
func writeUntrustedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "untrusted test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return writePEMCert(t, der)
}

func writePEMCert(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
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
	d.config = &config.Config{}
	client := &http.Client{Timeout: 100 * time.Millisecond}
	assert.False(t, d.probePeerHealth(client, config.Config{}))
	assert.False(t, d.probePeerHealth(client, config.Config{Host: "1.2.3.4"}))                                                       // no port available anywhere
	assert.False(t, d.probePeerHealth(client, config.Config{Host: "1.2.3.4", Daemon: config.DaemonConfig{Host: "not-a-host-port"}})) // unparseable + no self fallback
}

// TestPeerDaemonPort_FallbackToSelf — admin init renders [nodes.X.daemon]
// only for the local node, so peer.Daemon.Host is usually empty. The peer
// probe must reuse the local daemon's port (clusters are symmetric).
func TestPeerDaemonPort_FallbackToSelf(t *testing.T) {
	d := &Daemon{config: &config.Config{Daemon: config.DaemonConfig{Host: "0.0.0.0:4432"}}}

	// Peer with no Daemon block — use local port.
	assert.Equal(t, "4432", d.peerDaemonPort(config.Config{Host: "10.0.0.2"}))

	// Peer with explicit Daemon.Host — prefer the explicit value.
	assert.Equal(t, "8443", d.peerDaemonPort(config.Config{
		Host:   "10.0.0.2",
		Daemon: config.DaemonConfig{Host: "0.0.0.0:8443"},
	}))

	// Unparseable peer Daemon.Host + no self config — empty.
	d2 := &Daemon{}
	assert.Empty(t, d2.peerDaemonPort(config.Config{Daemon: config.DaemonConfig{Host: "garbage"}}))
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
	d.config = &config.Config{NATS: config.NATSConfig{CACert: f.caPath}}

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

// TestMonitorPeerReachability_UntrustedPeerCert asserts a peer whose cert the
// cluster CA did not sign counts as unreachable, even while serving 200.
func TestMonitorPeerReachability_UntrustedPeerCert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ticker-driven peer probe test in short mode")
	}
	f := newPeerHealthFixture(t, 1)
	d, cancel := daemonForPeerHealth(t, f.cfg)
	d.config = &config.Config{NATS: config.NATSConfig{CACert: writeUntrustedCA(t)}}
	d.peersReachable.Store(true)

	go d.monitorPeerReachability()
	defer cancel()

	require.Eventually(t, func() bool { return !d.peersReachable.Load() },
		peerProbeTimeout+time.Second, 50*time.Millisecond,
		"a peer presenting an untrusted cert must not count as reachable")
}

// TestMonitorPeerReachability_NoCAKeepsPeersReachable asserts a node that
// cannot load the cluster CA stops probing rather than declaring every peer
// unreachable and dropping out of cluster mode.
func TestMonitorPeerReachability_NoCAKeepsPeersReachable(t *testing.T) {
	f := newPeerHealthFixture(t, 2)
	for _, p := range f.peers {
		p.setHealthy(false)
	}
	d, _ := daemonForPeerHealth(t, f.cfg)
	d.config = &config.Config{}
	d.peersReachable.Store(true)

	done := make(chan struct{})
	go func() {
		d.monitorPeerReachability()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorPeerReachability kept running without a cluster CA")
	}
	assert.True(t, d.peersReachable.Load())
}
