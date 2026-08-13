package daemon

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"path/filepath"
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
// that does run a meta node.
func computePredastoreHealth(ctx context.Context, d *Daemon) string {
	nodes, cfg, err := localMetaNodes(d)
	if err != nil {
		slog.Warn("predastore health probe: could not resolve local meta nodes", "err", err)
		return predastoreHealthUnreachable
	}
	if len(nodes) == 0 {
		return predastoreHealthOK
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

// localMetaNodes returns the meta node ids pinned to this daemon's predastore
// host, and the config they were parsed from. A node with no predastore host
// id configured — this node runs no predastore at all — returns an empty
// slice and no error, which computePredastoreHealth treats as "ok" rather
// than "unreachable": it is not this host's job to answer for a service it
// was never asked to run.
func localMetaNodes(d *Daemon) ([]pds.NodeID, *pds.Config, error) {
	if d.config.Predastore.HostID <= 0 {
		return nil, nil, nil
	}

	cfgPath := predastoreConfigPath(d.configPath)
	cfg, err := pds.LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load predastore config %s: %w", cfgPath, err)
	}

	nodes := pds.MetaNodesOnHost(cfg, pds.HostID(d.config.Predastore.HostID))
	return nodes, cfg, nil
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
