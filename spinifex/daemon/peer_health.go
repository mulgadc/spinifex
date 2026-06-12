package daemon

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
)

// Peer-health probe cadence. Worst-case partition detection on a 3-node
// cluster is ~6s — within the 30s DDIL Scenario C budget.
const (
	peerProbeInterval = 2 * time.Second
	peerProbeTimeout  = 2 * time.Second
)

// peerCount returns the number of peers (other nodes) in the cluster config.
// Zero when the daemon is single-node or the cluster config is nil.
func (d *Daemon) peerCount() int {
	if d.clusterConfig == nil {
		return 0
	}
	n := len(d.clusterConfig.Nodes)
	if _, self := d.clusterConfig.Nodes[d.clusterConfig.Node]; self && n > 0 {
		n--
	}
	return n
}

// peerNodes returns every node in the cluster config except this one. Order
// is map-iteration order — callers must not depend on stability.
func (d *Daemon) peerNodes() []config.Config {
	if d.clusterConfig == nil {
		return nil
	}
	out := make([]config.Config, 0, len(d.clusterConfig.Nodes))
	for name, n := range d.clusterConfig.Nodes {
		if name == d.clusterConfig.Node {
			continue
		}
		out = append(out, n)
	}
	return out
}

// monitorPeerReachability polls every peer's /health and updates peersReachable.
// No-op on single-node. Probes are serial: any one responding suffices.
func (d *Daemon) monitorPeerReachability() {
	peers := d.peerNodes()
	if len(peers) == 0 {
		return
	}

	client := &http.Client{
		Timeout: peerProbeTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed per-node certs (matches harness DaemonClient)
		},
	}
	defer client.CloseIdleConnections()

	t := time.NewTicker(peerProbeInterval)
	defer t.Stop()

	d.probePeersOnce(client, peers)
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
			d.probePeersOnce(client, peers)
		}
	}
}

// probePeersOnce probes peers serially, short-circuiting on the first healthy
// response. Sets peersReachable to the outcome.
func (d *Daemon) probePeersOnce(client *http.Client, peers []config.Config) {
	reachable := false
	for _, p := range peers {
		if d.probePeerHealth(client, p) {
			reachable = true
			break
		}
	}
	prev := d.peersReachable.Swap(reachable)
	if prev != reachable {
		slog.Info("peer reachability changed", "reachable", reachable, "peers", len(peers))
		if reachable {
			// DDIL Scenario C: NATS stayed connected, so this flip is
			// the only heal signal. Goroutine keeps the ticker non-blocking.
			go d.reconcileOnHeal("peer-probe-heal")
		}
	}
}

// peerDaemonPort returns the TCP port to dial on peer p. Falls back to the
// local daemon's port since remote-node config blocks typically omit it.
func (d *Daemon) peerDaemonPort(p config.Config) string {
	if p.Daemon.Host != "" {
		if _, port, err := net.SplitHostPort(p.Daemon.Host); err == nil && port != "" {
			return port
		}
	}
	if d.config == nil || d.config.Daemon.Host == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(d.config.Daemon.Host)
	if err != nil {
		return ""
	}
	return port
}

// probePeerHealth issues a single short-timeout GET /health against peer p.
// Returns true iff the peer responded with a 2xx status. Connection errors,
// TLS failures, timeouts, and non-2xx responses all count as unreachable.
func (d *Daemon) probePeerHealth(client *http.Client, p config.Config) bool {
	addr := p.AdvertiseIP
	if addr == "" {
		addr = p.Host
	}
	if addr == "" {
		return false
	}
	port := d.peerDaemonPort(p)
	if port == "" {
		return false
	}
	url := "https://" + net.JoinHostPort(addr, port) + "/health"

	ctx, cancel := context.WithTimeout(d.ctx, peerProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
