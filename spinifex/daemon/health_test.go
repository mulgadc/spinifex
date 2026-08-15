// Exercises unexported probe internals (serviceProbes, computePredastoreHealth,
// nodeStatusFn, predastoreHealthCache) that have no exported surface to drive
// them through.
//
//test:in-package
package daemon

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthTestTOML pins one meta node (id 3) to host 1, and gives host 2 no
// meta node at all — the two shapes localMetaNodes has to tell apart.
const healthTestTOML = `
version = 1
region = "ap-southeast-2"

[rs]
data = 1
parity = 1

[[host]]
id = 1
bind_addr = "0.0.0.0"
addr = "10.0.0.1"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host.node]]
id = 1
role = "gate"
port = 8443

[[host.node]]
id = 2
role = "blob"
port = 6660

[[host.node]]
id = 3
role = "meta"
port = 7660

[[host]]
id = 2
bind_addr = "0.0.0.0"
addr = "10.0.0.2"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host.node]]
id = 4
role = "blob"
port = 6660
`

// singleHostTOML runs every node in one [[host]] — a gate, a blob and the
// meta node — so the meta node is pipe-only and the probe must fall back to
// the gate rather than the QUIC raft dial.
const singleHostTOML = `
version = 1
region = "ap-southeast-2"

[rs]
data = 1
parity = 0

[[host]]
id = 1
bind_addr = "127.0.0.1"
addr = "127.0.0.1"
data_dir = "/var/lib/spinifex/predastore/cluster"

[[host.node]]
id = 1
role = "gate"
port = 8443
bind_addr = "0.0.0.0"

[[host.node]]
id = 2
role = "blob"
port = 6660

[[host.node]]
id = 3
role = "meta"
port = 7660
`

// newHealthTestDaemon writes healthTestTOML and a CA cert under a temp
// config dir and returns a Daemon whose predastore host id is hostID.
func newHealthTestDaemon(t *testing.T, hostID int) *Daemon {
	t.Helper()
	return newHealthTestDaemonTOML(t, hostID, healthTestTOML)
}

// newHealthTestDaemonTOML is newHealthTestDaemon for an arbitrary predastore
// config, so a test can pick single- or multi-host topology.
func newHealthTestDaemonTOML(t *testing.T, hostID int, toml string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "predastore"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "predastore", "predastore.toml"), []byte(toml), 0o600))

	certPEM, _ := generateTestCert(t)
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, certPEM, 0o600))

	return &Daemon{
		configPath: filepath.Join(dir, "spinifex.toml"),
		config: &config.Config{
			Predastore: config.PredastoreConfig{HostID: hostID},
			NATS:       config.NATSConfig{CACert: caPath},
		},
	}
}

// setGateDialFn stubs gateDialFn for the test's lifetime and returns a pointer
// to a call counter, mirroring setNodeStatusFn.
func setGateDialFn(t *testing.T, fn func(context.Context, string, string) (net.Conn, error)) *int {
	t.Helper()
	calls := 0
	orig := gateDialFn
	gateDialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		return fn(ctx, network, addr)
	}
	t.Cleanup(func() { gateDialFn = orig })
	return &calls
}

// setNodeStatusFn stubs nodeStatusFn for the test's lifetime, restoring the
// real pds.NodeStatus on cleanup, and returns a pointer to a call counter.
func setNodeStatusFn(t *testing.T, fn func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error)) *int {
	t.Helper()
	calls := 0
	orig := nodeStatusFn
	nodeStatusFn = func(ctx context.Context, cfg *pds.Config, node pds.NodeID, rootCAs *x509.CertPool) (pds.Status, error) {
		calls++
		return fn(ctx, cfg, node, rootCAs)
	}
	t.Cleanup(func() { nodeStatusFn = orig })
	return &calls
}

func TestProbeServiceHealth_UnprobedServiceDefaultsToOK(t *testing.T) {
	d := &Daemon{config: &config.Config{Services: []string{"viperblock"}}}
	health := d.probeServiceHealth(t.Context())
	assert.Equal(t, "ok", health["viperblock"])
}

func TestComputePredastoreHealth_NoMetaNodeOnHost(t *testing.T) {
	// Host 2 in healthTestTOML runs only a blob node — nothing local to
	// probe, which is not this host's service failing.
	d := newHealthTestDaemon(t, 2)
	calls := setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{}, nil
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthOK, got)
	assert.Equal(t, 0, *calls, "a host with no meta node must never dial one")
}

func TestComputePredastoreHealth_NoHostIDConfigured(t *testing.T) {
	// HostID <= 0: this node runs no predastore at all.
	d := newHealthTestDaemon(t, 0)
	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthOK, got)
}

func TestComputePredastoreHealth_OK(t *testing.T) {
	d := newHealthTestDaemon(t, 1)
	setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{State: "Leader", Leader: "node-3", IsLeader: true}, nil
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthOK, got)
}

func TestComputePredastoreHealth_NoLeader(t *testing.T) {
	d := newHealthTestDaemon(t, 1)
	setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{State: "Follower", Leader: ""}, nil
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthNoLeader, got)
}

func TestComputePredastoreHealth_UnreachableOnError(t *testing.T) {
	d := newHealthTestDaemon(t, 1)
	setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{}, assert.AnError
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthUnreachable, got)
}

func TestComputePredastoreHealth_UnreachableOnTimeout(t *testing.T) {
	d := newHealthTestDaemon(t, 1)

	// Shrink the probe timeout so a wedged peer is provably bounded rather
	// than actually waiting the production default.
	origTimeout := predastoreProbeTimeout
	predastoreProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { predastoreProbeTimeout = origTimeout })

	setNodeStatusFn(t, func(ctx context.Context, cfg *pds.Config, node pds.NodeID, rootCAs *x509.CertPool) (pds.Status, error) {
		<-ctx.Done()
		return pds.Status{}, ctx.Err()
	})

	start := time.Now()
	got := computePredastoreHealth(t.Context(), d)
	elapsed := time.Since(start)

	assert.Equal(t, predastoreHealthUnreachable, got)
	assert.Less(t, elapsed, 2*time.Second, "a wedged peer must not stall the handler beyond the probe timeout")
}

func TestComputePredastoreHealth_NoCAConfigured(t *testing.T) {
	d := newHealthTestDaemon(t, 1)
	d.config.NATS.CACert = ""

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthUnreachable, got)
}

func TestComputePredastoreHealth_SingleHostGateOK(t *testing.T) {
	// Single-host: the meta node is pipe-only, so the probe must dial the gate
	// and never the raft QUIC endpoint.
	d := newHealthTestDaemonTOML(t, 1, singleHostTOML)

	metaCalls := setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{}, assert.AnError
	})
	gateCalls := setGateDialFn(t, func(context.Context, string, string) (net.Conn, error) {
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return client, nil
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthOK, got)
	assert.Equal(t, 1, *gateCalls, "single-host must probe the gate")
	assert.Equal(t, 0, *metaCalls, "single-host must never QUIC-dial the meta node")
}

func TestComputePredastoreHealth_SingleHostGateUnreachable(t *testing.T) {
	d := newHealthTestDaemonTOML(t, 1, singleHostTOML)
	setGateDialFn(t, func(context.Context, string, string) (net.Conn, error) {
		return nil, assert.AnError
	})

	got := computePredastoreHealth(t.Context(), d)
	assert.Equal(t, predastoreHealthUnreachable, got)
}

func TestLocalGateAddr(t *testing.T) {
	// A wildcard gate bind is dialled on the loopback the daemon shares.
	cfg := &pds.Config{Hosts: []pds.HostConfig{{
		ID: 1,
		Nodes: []pds.NodeConfig{
			{ID: 3, Role: pds.RoleMeta, Port: 7660},
			{ID: 1, Role: pds.RoleGate, Port: 8443, BindAddr: "0.0.0.0"},
		},
	}}}
	addr, ok := localGateAddr(cfg, 1)
	require.True(t, ok)
	assert.Equal(t, "127.0.0.1:8443", addr)

	// A host running a meta node but no gate has no socket to probe.
	noGate := &pds.Config{Hosts: []pds.HostConfig{{
		ID:    1,
		Nodes: []pds.NodeConfig{{ID: 3, Role: pds.RoleMeta, Port: 7660}},
	}}}
	_, ok = localGateAddr(noGate, 1)
	assert.False(t, ok)
}

func TestProbePredastore_CachesWithinTTL(t *testing.T) {
	d := newHealthTestDaemon(t, 1)

	origTTL := predastoreHealthCacheTTL
	predastoreHealthCacheTTL = time.Minute
	t.Cleanup(func() { predastoreHealthCacheTTL = origTTL })

	calls := setNodeStatusFn(t, func(context.Context, *pds.Config, pds.NodeID, *x509.CertPool) (pds.Status, error) {
		return pds.Status{State: "Leader", Leader: "node-3"}, nil
	})

	first := probePredastore(t.Context(), d)
	second := probePredastore(t.Context(), d)

	assert.Equal(t, predastoreHealthOK, first)
	assert.Equal(t, predastoreHealthOK, second)
	assert.Equal(t, 1, *calls, "a second poll within the TTL must not re-dial")
}
