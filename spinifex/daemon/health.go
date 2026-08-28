package daemon

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Predastore health verdicts, as reported under service_health.predastore in
// the /health response.
const (
	predastoreHealthOK          = "ok"
	predastoreHealthNoLeader    = "no_leader"
	predastoreHealthUnreachable = "unreachable"
)

// predastoreProbeTimeout bounds every dial a /health request makes to this
// host's own meta node(s), so a wedged or unreachable peer can never stall
// the handler. A var, not a const, so tests can shrink it to exercise the
// timeout path without a multi-second sleep.
var predastoreProbeTimeout = 2 * time.Second

// predastoreHealthCacheTTL bounds how stale a cached predastore verdict may
// be. /health is a monitoring target polled far more often than raft state
// changes, and a fresh QUIC dial per poll would put that polling load on the
// raft plane; a few seconds of staleness is a good trade for that. A var for
// the same reason as predastoreProbeTimeout.
var predastoreHealthCacheTTL = 5 * time.Second

// nodeStatusFn is pds.NodeStatus, indirected so tests can stub the raft dial
// and exercise every verdict — including the timeout path — without a live
// predastore process.
var nodeStatusFn = pds.NodeStatus

// gateDialFn opens a TCP connection to a predastore S3 gate, indirected so
// tests can stub the dial and exercise the single-host verdicts without a live
// listener. A bare TCP connect is enough: it completes once the process is
// accepting, before any TLS, which is exactly "the predastore process is up".
var gateDialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// readyzFn performs the /readyz HTTP GET against a predastore admin listener
// and reports its status code, indirected so tests can stub the transport and
// exercise every verdict without a live listener.
var readyzFn = func(ctx context.Context, addr string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// probeFunc reports one service's real health, in place of the "ok" every
// service otherwise reports simply for being named in spinifex.toml.
type probeFunc func(ctx context.Context, d *Daemon) string

// serviceProbes maps a service name to the probe that reports its real
// health. A service absent from this map keeps reporting "ok" as soon as it
// is named in spinifex.toml: service_health.<svc> == "ok" gates several
// ansible bootstrap playbooks, and changing that default in the same change
// that adds the first probe would stall them for a reason unrelated to any
// bug in those services. Probes for the other services are a separate piece
// of work.
var serviceProbes = map[string]probeFunc{
	"predastore": probePredastore,
}

// probeServiceHealth reports every service named in spinifex.toml: a probed
// service returns its probe's verdict, and everything else returns "ok" —
// the same restatement of the config /health has always reported for
// services with no probe.
func (d *Daemon) probeServiceHealth(ctx context.Context) map[string]string {
	services := d.config.GetServices()
	health := make(map[string]string, len(services))
	for _, svc := range services {
		if probe, ok := serviceProbes[svc]; ok {
			health[svc] = probe(ctx, d)
			continue
		}
		health[svc] = "ok"
	}
	return health
}

// predastoreHealthCache holds the last predastore verdict this node computed,
// so concurrent or rapid /health polls do not each dial the local meta
// node(s) themselves.
type predastoreHealthCache struct {
	mu     sync.Mutex
	at     time.Time
	result string
}

// probePredastore reports the raft health of the meta node(s) MetaNodesOnHost
// pins to this host, serving a cached verdict when it is fresh enough.
func probePredastore(ctx context.Context, d *Daemon) string {
	d.predastoreHealth.mu.Lock()
	if !d.predastoreHealth.at.IsZero() && time.Since(d.predastoreHealth.at) < predastoreHealthCacheTTL {
		result := d.predastoreHealth.result
		d.predastoreHealth.mu.Unlock()
		return result
	}
	d.predastoreHealth.mu.Unlock()

	result := computePredastoreHealth(ctx, d)

	d.predastoreHealth.mu.Lock()
	d.predastoreHealth.result = result
	d.predastoreHealth.at = time.Now()
	d.predastoreHealth.mu.Unlock()

	return result
}

// computePredastoreHealth dials every meta node MetaNodesOnHost pins to this
// host and reports the worst of them: unreachable beats no_leader beats ok,
// since any one of them failing is this host's predastore service failing.
// A host that runs no meta node at all — a pure gate/blob host, or a node
// that runs no predastore — has nothing local to probe and reports ok: that
// is not a failure of this host's own service, and the raft quorum as a
// whole is what `spx admin storage status` reports on, across every host
// that does run a meta node. A single-host install has no reachable meta
// socket at all and falls back to probing the local gate — see probeLocalGate.
func computePredastoreHealth(ctx context.Context, d *Daemon) string {
	nodes, cfg, hostID, err := localMetaNodes(d)
	if err != nil {
		slog.Warn("predastore health probe: could not resolve local meta nodes", "err", err)
		return predastoreHealthUnreachable
	}
	if len(nodes) == 0 {
		// This host runs predastore but no meta node — nothing raft-local to
		// probe. Its admin listener, if named, still answers for the roles it
		// does run, so ask that rather than reporting ok unconditionally.
		if cfg != nil {
			if verdict, ok := probeReadyz(ctx, cfg, hostID); ok {
				return verdict
			}
		}
		return predastoreHealthOK
	}

	// Single-host: the meta node talks over an in-process pipe and binds no
	// QUIC socket, so this daemon — a separate process — can never dial it.
	// Probe predastore's one cross-process listener, the S3 gate, instead: a
	// gate that accepts a connection is a predastore process serving, and on a
	// single voter raft is trivially leader.
	if predastoreIsSingleHost(cfg, hostID) {
		return probeLocalGate(ctx, d, cfg, hostID)
	}

	rootCAs, err := loadPredastoreTrustRoot(d)
	if err != nil {
		slog.Warn("predastore health probe: could not load cluster CA", "err", err)
		return predastoreHealthUnreachable
	}

	probeCtx, cancel := context.WithTimeout(ctx, predastoreProbeTimeout)
	defer cancel()

	verdict := predastoreHealthOK
	for _, node := range nodes {
		status, err := nodeStatusFn(probeCtx, cfg, node, rootCAs)
		if err != nil {
			slog.Debug("predastore health probe: node unreachable", "node", node, "err", err)
			return predastoreHealthUnreachable
		}
		if status.Leader == "" {
			verdict = predastoreHealthNoLeader
		}
	}
	return verdict
}

// predastoreIsSingleHost reports whether no predastore node runs off this
// host, which is the only condition under which a local node opens a network
// socket. When none does, the meta node is pipe-only and unreachable to a
// separate process, so the raft QUIC probe cannot apply.
func predastoreIsSingleHost(cfg *pds.Config, hostID pds.HostID) bool {
	for _, h := range cfg.Hosts {
		if h.ID != hostID && len(h.Nodes) > 0 {
			return false
		}
	}
	return true
}

// probeLocalGate reports predastore's health on a single-host install. Its
// admin listener, when named, is asked first since it observes the meta and
// blob roles the pipe-only meta node cannot expose; otherwise this falls back
// to connecting to the local S3 gate, its one cross-process listener. A host
// running no gate exposes no socket to probe and reports ok rather than
// failing a service it cannot answer for on a channel that cannot exist.
func probeLocalGate(ctx context.Context, d *Daemon, cfg *pds.Config, hostID pds.HostID) string {
	if verdict, ok := probeReadyz(ctx, cfg, hostID); ok {
		return verdict
	}

	addr, ok := localGateAddr(cfg, hostID)
	if !ok {
		return predastoreHealthOK
	}

	probeCtx, cancel := context.WithTimeout(ctx, predastoreProbeTimeout)
	defer cancel()

	conn, err := gateDialFn(probeCtx, "tcp", addr)
	if err != nil {
		slog.Debug("predastore health probe: gate unreachable", "addr", addr, "err", err)
		return predastoreHealthUnreachable
	}
	_ = conn.Close()
	return predastoreHealthOK
}

// probeReadyz consults predastore's /readyz on this host's admin listener,
// when the host names one. 200 maps to ok; 503 maps to unreachable, the
// closest existing verdict for "the process is running but cannot serve".
// ok is false whenever no admin port is configured, the listener could not be
// reached at all, or it answered with neither status: in every case the
// caller must fall back to its own probe, so that an older predastore build
// with no admin listener is never read as unhealthy.
func probeReadyz(ctx context.Context, cfg *pds.Config, hostID pds.HostID) (string, bool) {
	addr, ok := localAdminAddr(cfg, hostID)
	if !ok {
		return "", false
	}

	probeCtx, cancel := context.WithTimeout(ctx, predastoreProbeTimeout)
	defer cancel()

	status, err := readyzFn(probeCtx, addr)
	if err != nil {
		slog.Debug("predastore health probe: admin listener unreachable, falling back", "addr", addr, "err", err)
		return "", false
	}
	switch status {
	case http.StatusOK:
		return predastoreHealthOK, true
	case http.StatusServiceUnavailable:
		return predastoreHealthUnreachable, true
	default:
		slog.Warn("predastore health probe: unexpected /readyz status, falling back", "addr", addr, "status", status)
		return "", false
	}
}

// localAdminAddr is the dial address of this host's predastore admin
// listener, and whether the host names one. A wildcard or empty bind_addr is
// dialled on the loopback, since the daemon and predastore share the host.
func localAdminAddr(cfg *pds.Config, hostID pds.HostID) (string, bool) {
	for _, h := range cfg.Hosts {
		if h.ID != hostID {
			continue
		}
		if h.AdminPort == 0 {
			return "", false
		}
		host := pds.HostBindAddr(h)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, strconv.Itoa(h.AdminPort)), true
	}
	return "", false
}

// localGateAddr is the dial address of the S3 gate on this host, and whether
// the host runs one. A wildcard or empty bind is dialled on the loopback,
// since the daemon and predastore share the host.
func localGateAddr(cfg *pds.Config, hostID pds.HostID) (string, bool) {
	for _, h := range cfg.Hosts {
		if h.ID != hostID {
			continue
		}
		for _, n := range h.Nodes {
			if n.Role != pds.RoleGate {
				continue
			}
			host := pds.NodeBindAddr(h, n)
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			return net.JoinHostPort(host, strconv.Itoa(n.Port)), true
		}
	}
	return "", false
}

// localMetaNodes returns the meta node ids pinned to this daemon's predastore
// host, and the config they were parsed from. A node with no predastore host
// id configured — this node runs no predastore at all — returns an empty
// slice and no error, which computePredastoreHealth treats as "ok" rather
// than "unreachable": it is not this host's job to answer for a service it
// was never asked to run.
func localMetaNodes(d *Daemon) ([]pds.NodeID, *pds.Config, pds.HostID, error) {
	if d.config.Predastore.HostID <= 0 {
		return nil, nil, 0, nil
	}

	cfgPath := predastoreConfigPath(d.configPath)
	cfg, err := pds.LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load predastore config %s: %w", cfgPath, err)
	}

	hostID := pds.HostID(d.config.Predastore.HostID)
	return pds.MetaNodesOnHost(cfg, hostID), cfg, hostID, nil
}

// predastoreConfigPath is where the predastore process reads its own config:
// a fixed "predastore/predastore.toml" alongside the daemon's own
// spinifex.toml, matching how the predastore service and handleStorageConfig
// both resolve it.
func predastoreConfigPath(daemonConfigPath string) string {
	return filepath.Join(filepath.Dir(daemonConfigPath), "predastore", "predastore.toml")
}

// loadPredastoreTrustRoot returns the cluster CA pool predastore's meta
// replicas are issued from — the same CA (d.config.NATS.CACert, conventionally
// /etc/spinifex/ca.pem) the daemon already trusts NATS peers with.
func loadPredastoreTrustRoot(d *Daemon) (*x509.CertPool, error) {
	if d.config.NATS.CACert == "" {
		return nil, fmt.Errorf("cluster CA not configured (nats.cacert)")
	}
	return utils.LoadCertPool(d.config.NATS.CACert)
}
