package predastore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/predastore/clusterrun"
	"github.com/mulgadc/predastore/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	roleShardStorage = "shard-storage"
	roleStateReplica = "state-replica"
)

// nodeIDSeq hands out node ids that are unique within this test binary. The
// pipe transport's listener registry is process-wide, and this package's
// integration tests hold a fixture daemon on the low ids for the life of the
// binary, so topologies here must not reuse them.
var nodeIDSeq atomic.Int64

func nextNodeIDs(n int) []int {
	ids := make([]int, n)
	for i := range ids {
		ids[i] = 100 + int(nodeIDSeq.Add(1))
	}
	return ids
}

// hostSpec and nodeSpec are one entry each of a topology's [[host]] and
// [[node]] tables.
type hostSpec struct {
	id      int
	dataDir string
}

type nodeSpec struct {
	id     int
	hostID int
	role   string
}

// writeTopology writes a predastore config for hosts and nodes, returning its
// path. Hosts bind an ephemeral port so several topologies can coexist in one
// test binary.
func writeTopology(t *testing.T, dir string, hosts []hostSpec, nodes []nodeSpec) string {
	t.Helper()

	var b strings.Builder
	fmt.Fprintf(&b, "version = \"1.0\"\nregion = \"us-east-1\"\nbase_path = %q\n\n[rs]\ndata = 1\nparity = 1\n", dir)
	for _, h := range hosts {
		fmt.Fprintf(&b, "\n[[host]]\nid = %d\nbind_addr = \"127.0.0.1:0\"\npublic_addr = \"127.0.0.1:0\"\ndata_dir = %q\n", h.id, h.dataDir)
	}
	for _, n := range nodes {
		fmt.Fprintf(&b, "\n[[node]]\nid = %d\nhost_id = %d\nrole = %q\n", n.id, n.hostID, n.role)
	}

	path := filepath.Join(dir, "predastore.toml")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0600))
	return path
}

// writeMasterKey writes a 32-byte master key at mode. The mode matters:
// masterkey.Load is fail-closed on anything group- or other-readable.
func writeMasterKey(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)

	path := filepath.Join(dir, "master.key")
	require.NoError(t, os.WriteFile(path, key, mode))
	return path
}

// writeCertificate writes a self-signed P-256 certificate serving both the
// intra-cluster QUIC socket and the S3 gateway. Nothing verifies it: the peers
// are this process and the tests' own dialers.
func writeCertificate(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "tls.crt")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPath = filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600))

	return certPath, keyPath
}

// freePort reserves and releases a loopback port, so a gateway under test
// binds somewhere the fixture daemon and other packages are not.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newTwoHostConfig returns the config for the first host of a two-host
// topology: the shard stores run here, the state replica belongs to the other
// host's process. Returns the local node ids and the remote one.
func newTwoHostConfig(t *testing.T) (cfg *Config, local []int, remote int) {
	t.Helper()

	dir := t.TempDir()
	certPath, keyPath := writeCertificate(t, dir)
	ids := nextNodeIDs(3)

	hosts := []hostSpec{
		{id: 1, dataDir: filepath.Join(dir, "host-1")},
		{id: 2, dataDir: filepath.Join(dir, "host-2")},
	}
	nodes := []nodeSpec{
		{id: ids[0], hostID: 1, role: roleShardStorage},
		{id: ids[1], hostID: 1, role: roleShardStorage},
		{id: ids[2], hostID: 2, role: roleStateReplica},
	}

	cfg = &Config{
		ConfigPath:        writeTopology(t, dir, hosts, nodes),
		Host:              "127.0.0.1",
		Port:              freePort(t),
		BasePath:          dir,
		TlsCert:           certPath,
		TlsKey:            keyPath,
		EncryptionKeyFile: writeMasterKey(t, dir, 0600),
		HostID:            1,
	}
	return cfg, ids[:2], ids[2]
}

// newSingleHostConfig returns the single-node deployment's config: one host
// running every node in this process, over the in-process pipe.
func newSingleHostConfig(t *testing.T) *Config {
	t.Helper()

	dir := t.TempDir()
	certPath, keyPath := writeCertificate(t, dir)
	ids := nextNodeIDs(3)

	hosts := []hostSpec{{id: 1, dataDir: filepath.Join(dir, "host-1")}}
	nodes := []nodeSpec{
		{id: ids[0], hostID: 1, role: roleShardStorage},
		{id: ids[1], hostID: 1, role: roleShardStorage},
		{id: ids[2], hostID: 1, role: roleStateReplica},
	}

	// HostID is left zero: this process is the whole cluster.
	return &Config{
		ConfigPath:        writeTopology(t, dir, hosts, nodes),
		Host:              "127.0.0.1",
		Port:              freePort(t),
		BasePath:          dir,
		TlsCert:           certPath,
		TlsKey:            keyPath,
		EncryptionKeyFile: writeMasterKey(t, dir, 0600),
	}
}

// newGateway builds the S3 server serve runs, on top of an already-built
// cluster runtime.
func newGateway(t *testing.T, cfg *Config, rt *clusterrun.Runtime) *s3.Server {
	t.Helper()

	server, err := s3.NewServer(
		s3.WithConfigPath(cfg.ConfigPath),
		s3.WithAddress(cfg.Host, cfg.Port),
		s3.WithTLS(cfg.TlsCert, cfg.TlsKey),
		s3.WithBasePath(cfg.BasePath),
		s3.WithEncryptionKeyFile(cfg.EncryptionKeyFile),
		s3.WithPreparedBackend(rt.Backend),
	)
	require.NoError(t, err)
	return server
}

// requireGatewayAccepts waits for the S3 gateway to accept connections, which
// is also the point at which Start has published the cancel func Shutdown
// needs.
func requireGatewayAccepts(t *testing.T, cfg *Config) {
	t.Helper()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 30*time.Second, 100*time.Millisecond, "gateway never accepted connections on %s", addr)
}

// TestBuildRuntimeErrors covers every way assembling the cluster runtime can
// fail before a single node is running.
func TestBuildRuntimeErrors(t *testing.T) {
	testCases := []struct {
		name    string
		config  func(t *testing.T, dir string) *Config
		wantErr string
	}{
		{
			name: "Unreadable Config",
			config: func(t *testing.T, dir string) *Config {
				return &Config{
					ConfigPath:        filepath.Join(dir, "absent.toml"),
					BasePath:          dir,
					EncryptionKeyFile: writeMasterKey(t, dir, 0600),
				}
			},
			wantErr: "read predastore config",
		},
		{
			name: "Host Owns No Nodes",
			config: func(t *testing.T, dir string) *Config {
				ids := nextNodeIDs(1)
				return &Config{
					ConfigPath: writeTopology(t, dir,
						[]hostSpec{{id: 1, dataDir: filepath.Join(dir, "host-1")}},
						[]nodeSpec{{id: ids[0], hostID: 1, role: roleStateReplica}}),
					BasePath:          dir,
					EncryptionKeyFile: writeMasterKey(t, dir, 0600),
					HostID:            9,
				}
			},
			wantErr: "predastore host 9 has no nodes in",
		},
		{
			name: "Missing Master Key",
			config: func(t *testing.T, dir string) *Config {
				ids := nextNodeIDs(1)
				return &Config{
					ConfigPath: writeTopology(t, dir,
						[]hostSpec{{id: 1, dataDir: filepath.Join(dir, "host-1")}},
						[]nodeSpec{{id: ids[0], hostID: 1, role: roleStateReplica}}),
					BasePath:          dir,
					EncryptionKeyFile: filepath.Join(dir, "absent.key"),
				}
			},
			wantErr: "load predastore master key",
		},
		{
			name: "World Readable Master Key",
			config: func(t *testing.T, dir string) *Config {
				ids := nextNodeIDs(1)
				return &Config{
					ConfigPath: writeTopology(t, dir,
						[]hostSpec{{id: 1, dataDir: filepath.Join(dir, "host-1")}},
						[]nodeSpec{{id: ids[0], hostID: 1, role: roleStateReplica}}),
					BasePath:          dir,
					EncryptionKeyFile: writeMasterKey(t, dir, 0644),
				}
			},
			wantErr: "load predastore master key",
		},
		{
			name: "Remote Nodes Without TLS Material",
			config: func(t *testing.T, dir string) *Config {
				ids := nextNodeIDs(2)
				return &Config{
					ConfigPath: writeTopology(t, dir,
						[]hostSpec{
							{id: 1, dataDir: filepath.Join(dir, "host-1")},
							{id: 2, dataDir: filepath.Join(dir, "host-2")},
						},
						[]nodeSpec{
							{id: ids[0], hostID: 1, role: roleShardStorage},
							{id: ids[1], hostID: 2, role: roleStateReplica},
						}),
					BasePath:          dir,
					EncryptionKeyFile: writeMasterKey(t, dir, 0600),
					HostID:            1,
					// TlsCert and TlsKey deliberately left empty: reaching the
					// other host needs the intra-cluster socket.
				}
			},
			wantErr: "build predastore cluster runtime",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.config(t, t.TempDir()))
			require.NoError(t, err)

			rt, err := svc.buildRuntime()

			require.Error(t, err)
			assert.Nil(t, rt)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestBuildRuntimeRunsOnlyThisHostsNodes verifies host selection: a host runs
// the nodes pinned to it and leaves the rest to the processes that own them.
func TestBuildRuntimeRunsOnlyThisHostsNodes(t *testing.T) {
	cfg, local, remote := newTwoHostConfig(t)

	svc, err := New(cfg)
	require.NoError(t, err)

	rt, err := svc.buildRuntime()
	require.NoError(t, err)
	t.Cleanup(rt.Close)

	assert.NotNil(t, rt.Backend)
	for _, id := range local {
		assert.DirExists(t, filepath.Join(cfg.BasePath, "host-1", fmt.Sprintf("node-%d", id)))
	}
	assert.NoDirExists(t, filepath.Join(cfg.BasePath, "host-2", fmt.Sprintf("node-%d", remote)))
}

// TestServeReportsGatewayFailures covers the two ways the S3 gateway can fail
// the service: refusing to start, and dying once started.
func TestServeReportsGatewayFailures(t *testing.T) {
	testCases := []struct {
		name    string
		tls     func(cfg *Config)
		wantErr string
	}{
		{
			name:    "Missing TLS Material",
			tls:     func(cfg *Config) { cfg.TlsCert, cfg.TlsKey = "", "" },
			wantErr: "start predastore s3 gateway",
		},
		{
			name: "Unloadable Certificate",
			tls: func(cfg *Config) {
				cfg.TlsCert = filepath.Join(cfg.BasePath, "absent.crt")
				cfg.TlsKey = filepath.Join(cfg.BasePath, "absent.key")
			},
			wantErr: "predastore s3 gateway",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTwoHostConfig(t)
			svc, err := New(cfg)
			require.NoError(t, err)

			rt, err := svc.buildRuntime()
			require.NoError(t, err)
			t.Cleanup(rt.Close)

			// The runtime is built first: reaching the other host's node needs
			// the certificate this case is about to take away.
			tc.tls(cfg)
			gateway := newGateway(t, cfg, rt)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err = svc.serve(ctx, gateway)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestServeDrainsOnContextCancel verifies the gateway stops serving when the
// service context is cancelled, rather than outliving the cluster beneath it.
func TestServeDrainsOnContextCancel(t *testing.T) {
	cfg, _, _ := newTwoHostConfig(t)
	svc, err := New(cfg)
	require.NoError(t, err)

	rt, err := svc.buildRuntime()
	require.NoError(t, err)
	t.Cleanup(rt.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newGateway(t, cfg, rt)
	served := make(chan error, 1)
	go func() { served <- svc.serve(ctx, gateway) }()

	requireGatewayAccepts(t, cfg)
	cancel()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
}

// TestStartFailsWhenGatewayCannotServe verifies a fatal gateway error takes
// the whole service down: a cluster left running headless would look healthy
// while serving nothing.
func TestStartFailsWhenGatewayCannotServe(t *testing.T) {
	cfg := newSingleHostConfig(t)
	cfg.TlsCert = filepath.Join(cfg.BasePath, "absent.crt")
	cfg.TlsKey = filepath.Join(cfg.BasePath, "absent.key")

	svc, err := New(cfg)
	require.NoError(t, err)

	type result struct {
		pid int
		err error
	}
	done := make(chan result, 1)
	go func() {
		pid, err := svc.Start()
		done <- result{pid: pid, err: err}
	}()

	select {
	case res := <-done:
		require.Error(t, res.err)
		assert.Zero(t, res.pid)
		assert.Contains(t, res.err.Error(), "predastore s3 gateway")
	case <-time.After(60 * time.Second):
		t.Fatal("Start did not return after the gateway failed")
	}
}

// TestStartServesUntilShutdown covers the service lifecycle end to end: Start
// brings the cluster nodes and the gateway up together, and Shutdown drains
// both by cancelling the context they share.
func TestStartServesUntilShutdown(t *testing.T) {
	cfg := newSingleHostConfig(t)
	svc, err := New(cfg)
	require.NoError(t, err)

	type result struct {
		pid int
		err error
	}
	done := make(chan result, 1)
	go func() {
		pid, err := svc.Start()
		done <- result{pid: pid, err: err}
	}()

	// Shutdown must not run before Start publishes its cancel func, or it
	// falls through to the pid file and signals this test binary.
	requireGatewayAccepts(t, cfg)
	require.NoError(t, svc.Shutdown())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, os.Getpid(), res.pid)
	case <-time.After(60 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

// TestShutdownWithoutRunningCluster verifies the fallback path: with no
// cluster in this process there is no context to cancel, so Shutdown goes
// through the pid file instead.
func TestShutdownWithoutRunningCluster(t *testing.T) {
	svc, err := New(&Config{BasePath: t.TempDir()})
	require.NoError(t, err)

	// No pid file was ever written, so this reports rather than signals.
	assert.Error(t, svc.Shutdown())
}
