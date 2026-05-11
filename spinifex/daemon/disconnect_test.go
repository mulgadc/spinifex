package daemon

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDaemonModeDefaultStandalone — Mode() returns standalone before Start().
func TestDaemonModeDefaultStandalone(t *testing.T) {
	clusterCfg := &config.ClusterConfig{
		Node:  "node-1",
		Nodes: map[string]config.Config{"node-1": {}},
	}
	d, err := NewDaemon(clusterCfg)
	require.NoError(t, err)

	assert.Equal(t, DaemonModeStandalone, d.Mode())
	assert.Equal(t, int64(0), d.NATSRetryCount())
}

// TestDaemonModeFlipsOnConnectAndDisconnect — connecting flips to cluster,
// killing NATS flips back to standalone.
func TestDaemonModeFlipsOnConnectAndDisconnect(t *testing.T) {
	port := freePortForTest(t)
	ns := startTestNATSOnPortForTest(t, port)

	clusterCfg := &config.ClusterConfig{
		Node:  "node-1",
		Nodes: map[string]config.Config{"node-1": {NATS: config.NATSConfig{Host: ns.ClientURL()}}},
	}
	d, err := NewDaemon(clusterCfg)
	require.NoError(t, err)

	require.NoError(t, d.connectNATS())
	defer d.natsConn.Close()
	assert.Equal(t, DaemonModeCluster, d.Mode())

	ns.Shutdown()
	require.Eventually(t, func() bool { return d.Mode() == DaemonModeStandalone }, 3*time.Second, 50*time.Millisecond,
		"mode should flip to standalone after NATS shutdown")
}

// TestDaemonReconnectBumpsRetryCount — reconnect callback fires, count++.
func TestDaemonReconnectBumpsRetryCount(t *testing.T) {
	port := freePortForTest(t)
	ns := startTestNATSOnPortForTest(t, port)

	clusterCfg := &config.ClusterConfig{
		Node:  "node-1",
		Nodes: map[string]config.Config{"node-1": {NATS: config.NATSConfig{Host: ns.ClientURL()}}},
	}
	d, err := NewDaemon(clusterCfg)
	require.NoError(t, err)

	require.NoError(t, d.connectNATS())
	defer d.natsConn.Close()
	require.Equal(t, int64(0), d.NATSRetryCount())

	ns.Shutdown()
	require.Eventually(t, func() bool { return !d.natsConn.IsConnected() }, 3*time.Second, 50*time.Millisecond)

	startTestNATSOnPortForTest(t, port)
	require.Eventually(t, func() bool { return d.Mode() == DaemonModeCluster }, 5*time.Second, 50*time.Millisecond,
		"mode should flip back to cluster on reconnect")
	assert.GreaterOrEqual(t, d.NATSRetryCount(), int64(1))
}

func freePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	require.NoError(t, l.Close())
	return port
}

func startTestNATSOnPortForTest(t *testing.T, port int) *server.Server {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })
	return ns
}

// TestMode_NilStored_ReturnsStandalone — bare Daemon with no mode stored returns standalone.
// Covers the d.mode.Load() == nil branch in Mode().
func TestMode_NilStored_ReturnsStandalone(t *testing.T) {
	d := &Daemon{}
	assert.Equal(t, DaemonModeStandalone, d.Mode())
}

// TestMode_WrongTypeStored_ReturnsStandalone — non-string in atomic.Value falls
// back to standalone instead of panicking. Covers the type-assertion !ok branch.
func TestMode_WrongTypeStored_ReturnsStandalone(t *testing.T) {
	d := &Daemon{}
	d.mode.Store(struct{ x string }{x: "not a string"})
	assert.Equal(t, DaemonModeStandalone, d.Mode())
}

// TestOnNATSDisconnect_FlipsMode — direct unit test of the disconnect callback
// without needing an actual NATS disconnect roundtrip.
func TestOnNATSDisconnect_FlipsMode(t *testing.T) {
	d := &Daemon{}
	d.mode.Store(DaemonModeCluster)
	d.onNATSDisconnect(nil, nil)
	assert.Equal(t, DaemonModeStandalone, d.Mode())
}

// TestOnNATSReconnect_NoJetStreamManager_DoesNotPanic — reconnect with
// jsManager nil flips mode + bumps counter but skips the goroutine WriteState.
func TestOnNATSReconnect_NoJetStreamManager_DoesNotPanic(t *testing.T) {
	d := &Daemon{}
	d.mode.Store(DaemonModeStandalone)

	d.onNATSReconnect(nil)

	assert.Equal(t, DaemonModeCluster, d.Mode())
	assert.Equal(t, int64(1), d.NATSRetryCount())
}

// TestLocalStatePath_NilConfig_UsesDefault — defensive path used by callers
// that build a Daemon without populating Config (older test fixtures, etc).
func TestLocalStatePath_NilConfig_UsesDefault(t *testing.T) {
	d := &Daemon{}
	assert.Equal(t, "/var/lib/spinifex/state/instance-state.json", d.localStatePath())
}

// TestLocalStatePath_RootedAtDataDir — verifies 1a's DataDir-rooted layout.
func TestLocalStatePath_RootedAtDataDir(t *testing.T) {
	d := &Daemon{config: &config.Config{DataDir: "/var/lib/spinifex/n1"}}
	assert.Equal(t, "/var/lib/spinifex/n1/state/instance-state.json", d.localStatePath())
}

// TestDaemonWriteState_LocalFile — WriteState persists to the configured DataDir
// even with a nil jsManager (best-effort KV is the second half; local file
// must succeed standalone).
func TestDaemonWriteState_LocalFile(t *testing.T) {
	dataDir := t.TempDir()
	d := &Daemon{
		config: &config.Config{DataDir: dataDir},
		vmMgr:  vm.NewManager(),
	}
	d.vmMgr.Insert(&vm.VM{ID: "i-w1", InstanceType: "t3.micro"})

	require.NoError(t, d.WriteState())

	state, err := ReadLocalState(filepath.Join(dataDir, "state", "instance-state.json"))
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, "t3.micro", state.VMS["i-w1"].InstanceType)
}

// TestDaemonLoadState_MissingFile — fresh-install path: empty map, no error.
func TestDaemonLoadState_MissingFile(t *testing.T) {
	d := &Daemon{
		config: &config.Config{DataDir: t.TempDir()},
		vmMgr:  vm.NewManager(),
	}
	require.NoError(t, d.LoadState())
	assert.Equal(t, 0, d.vmMgr.Count())
}

// TestDaemonLoadState_RoundTrip — write, then read back into a fresh daemon
// rooted at the same DataDir.
func TestDaemonLoadState_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	writer := &Daemon{
		config: &config.Config{DataDir: dataDir},
		vmMgr:  vm.NewManager(),
	}
	writer.vmMgr.Insert(&vm.VM{ID: "i-rt1", InstanceType: "m5.large"})
	require.NoError(t, writer.WriteState())

	reader := &Daemon{
		config: &config.Config{DataDir: dataDir},
		vmMgr:  vm.NewManager(),
	}
	require.NoError(t, reader.LoadState())
	assert.Equal(t, 1, reader.vmMgr.Count())
	got, ok := reader.vmMgr.Get("i-rt1")
	require.True(t, ok)
	assert.Equal(t, "m5.large", got.InstanceType)
}

// TestDaemonLoadState_CorruptFile — corruption is fatal, daemon refuses start.
func TestDaemonLoadState_CorruptFile(t *testing.T) {
	dataDir := t.TempDir()
	d := &Daemon{config: &config.Config{DataDir: dataDir}}

	path := d.localStatePath()
	require.NoError(t, writeCorruptStateFile(path))

	err := d.LoadState()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read local state")
}

func writeCorruptStateFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{not json"), 0o600)
}

// TestStartLocal_SucceedsWithoutNATS — §1d: startLocal() brings the daemon up
// (cluster manager HTTPS, local state load, ready flag) without contacting
// NATS. /local/status reports standalone mode immediately.
func TestStartLocal_SucceedsWithoutNATS(t *testing.T) {
	tmpDir := t.TempDir()
	certPEM, keyPEM := generateTestCert(t)
	certPath := filepath.Join(tmpDir, "server.pem")
	keyPath := filepath.Join(tmpDir, "server.key")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))

	addr, err := freeAddrForTest()
	require.NoError(t, err)

	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {
				BaseDir: tmpDir,
				DataDir: tmpDir,
				NATS:    config.NATSConfig{Host: "nats://127.0.0.1:1"}, // unreachable
				Daemon: config.DaemonConfig{
					Host:    addr,
					TLSCert: certPath,
					TLSKey:  keyPath,
				},
			},
		},
	}

	d, err := NewDaemon(cfg)
	require.NoError(t, err)
	d.configPath = filepath.Join(tmpDir, "spinifex.toml")

	require.NoError(t, d.startLocal())
	defer func() {
		if d.clusterServer != nil {
			_ = d.clusterServer.Close()
		}
	}()

	assert.True(t, d.ready.Load(), "ready flag should be set after startLocal")
	assert.Equal(t, DaemonModeStandalone, d.Mode())

	// /local/status served via the cluster manager TLS listener.
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   2 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("https://%s/local/status", addr))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body LocalStatus
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, DaemonModeStandalone, body.Mode)
	assert.Equal(t, natsDisconnected, body.NATS)
}

// TestAssertNoClusterServicesInitialised_GuardsTier1Invariant — §1e-audit:
// the invariant guard at the end of startLocal must reject any state where a
// cluster-scoped handle has been initialised before startCluster runs. A
// future edit that hoists JetStream/KV-touching init into startLocal would
// re-introduce the boot-time NATS dependency that 1d removed; this test
// pins the regression.
func TestAssertNoClusterServicesInitialised_GuardsTier1Invariant(t *testing.T) {
	clean := &Daemon{}
	require.NoError(t, clean.assertNoClusterServicesInitialised(),
		"freshly-constructed daemon must satisfy the Tier 1 invariant")

	dirty := &Daemon{jsManager: &JetStreamManager{}}
	err := dirty.assertNoClusterServicesInitialised()
	require.Error(t, err, "non-nil cluster handle must trip the invariant")
	assert.Contains(t, err.Error(), "jsManager")
}

// TestStartCluster_StrictRequireNATS_BoundedAndExits — §1d-strict:
// SPINIFEX_REQUIRE_NATS=1 caps the first connect to requireNATSTimeout and
// invokes exitFunc(1) when the bounded retry expires. Validates the dev/test
// fail-fast path without flipping the prod default.
func TestStartCluster_StrictRequireNATS_BoundedAndExits(t *testing.T) {
	t.Setenv("SPINIFEX_REQUIRE_NATS", "1")

	tmpDir := t.TempDir()
	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {
				BaseDir: tmpDir,
				DataDir: tmpDir,
				NATS:    config.NATSConfig{Host: "nats://127.0.0.1:1"}, // unreachable
			},
		},
	}

	d, err := NewDaemon(cfg)
	require.NoError(t, err)
	d.requireNATSTimeout = 200 * time.Millisecond
	d.natsRetryOpts = []utils.RetryOption{utils.WithRetryDelay(20 * time.Millisecond)}

	exitCh := make(chan int, 1)
	d.exitFunc = func(code int) { exitCh <- code }

	done := make(chan error, 1)
	go func() { done <- d.startCluster() }()

	select {
	case code := <-exitCh:
		assert.Equal(t, 1, code, "strict-mode timeout must request exit(1)")
	case <-time.After(3 * time.Second):
		t.Fatal("strict-mode startCluster did not invoke exitFunc within deadline")
	}

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connect NATS (strict)")
	case <-time.After(3 * time.Second):
		t.Fatal("startCluster did not return after exitFunc was called")
	}
}

// TestStartCluster_NoStrictEnv_UsesInfiniteRetry — §1d-strict:
// without SPINIFEX_REQUIRE_NATS, startCluster keeps the Tier 1 infinite-retry
// path. exitFunc must not fire, even when NATS is unreachable. Cancellation
// via d.ctx is the only way out.
func TestStartCluster_NoStrictEnv_UsesInfiniteRetry(t *testing.T) {
	t.Setenv("SPINIFEX_REQUIRE_NATS", "")

	tmpDir := t.TempDir()
	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {
				BaseDir: tmpDir,
				DataDir: tmpDir,
				NATS:    config.NATSConfig{Host: "nats://127.0.0.1:1"}, // unreachable
			},
		},
	}

	d, err := NewDaemon(cfg)
	require.NoError(t, err)
	d.natsRetryOpts = []utils.RetryOption{utils.WithRetryDelay(20 * time.Millisecond)}

	exitCalled := make(chan int, 1)
	d.exitFunc = func(code int) { exitCalled <- code }

	done := make(chan error, 1)
	go func() { done <- d.startCluster() }()

	// Give the loop time to attempt several retries; it must not exit.
	select {
	case code := <-exitCalled:
		t.Fatalf("default mode must not invoke exitFunc; got code=%d", code)
	case <-time.After(300 * time.Millisecond):
	}

	d.cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connect NATS")
		assert.NotContains(t, err.Error(), "strict")
	case <-time.After(3 * time.Second):
		t.Fatal("startCluster did not return after ctx cancellation")
	}
}

// TestStartCluster_RetriesUntilContextCancelled — §1d: startCluster's NATS
// connect loop honours d.ctx. Cancelling the daemon context unblocks an
// otherwise-infinite retry.
func TestStartCluster_RetriesUntilContextCancelled(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {
				BaseDir: tmpDir,
				DataDir: tmpDir,
				NATS:    config.NATSConfig{Host: "nats://127.0.0.1:1"}, // unreachable
			},
		},
	}

	d, err := NewDaemon(cfg)
	require.NoError(t, err)
	// Bound retry delay so ctx-cancel unblocks promptly.
	d.natsRetryOpts = []utils.RetryOption{utils.WithRetryDelay(50 * time.Millisecond)}

	done := make(chan error, 1)
	go func() { done <- d.startCluster() }()

	time.Sleep(200 * time.Millisecond)
	d.cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connect NATS")
	case <-time.After(3 * time.Second):
		t.Fatal("startCluster did not return after ctx cancellation")
	}
}

func freeAddrForTest() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr, nil
}
