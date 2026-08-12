package predastore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostSpec and nodeSpec are the [[host]] and [[host.node]] tables of a
// configuration file under test. An empty field is left out of the file, which
// is how a test leaves a host-local value for the service flags to supply.
type hostSpec struct {
	id       int
	bindAddr string
	addr     string
	dataDir  string
	tlsCert  string
	tlsKey   string
}

type nodeSpec struct {
	id   int
	role string
	port int
}

// writeTopology writes a predastore configuration file for host and its nodes
// and returns its path. RS(1,0) is the narrowest code the file format allows,
// so one blob node satisfies the stripe-width check.
func writeTopology(t *testing.T, dir string, host hostSpec, nodes []nodeSpec) string {
	t.Helper()

	var b strings.Builder
	fmt.Fprint(&b, "version = 1\nregion = \"us-east-1\"\n\n[rs]\ndata = 1\nparity = 0\n\n")
	fmt.Fprintf(&b, "[[host]]\nid = %d\n", host.id)
	for _, field := range [][2]string{
		{"bind_addr", host.bindAddr},
		{"addr", host.addr},
		{"data_dir", host.dataDir},
		{"tls_cert", host.tlsCert},
		{"tls_key", host.tlsKey},
	} {
		if field[1] != "" {
			fmt.Fprintf(&b, "%s = %q\n", field[0], field[1])
		}
	}
	for _, n := range nodes {
		fmt.Fprintf(&b, "\n[[host.node]]\nid = %d\nrole = %q\nport = %d\n", n.id, n.role, n.port)
	}

	path := filepath.Join(dir, "predastore.toml")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0600))
	return path
}

// singleHost is the topology most of these tests merge against: a gate and the
// one blob node RS(1,0) needs, on a host that names every host-local field so
// a flag has something to beat.
func singleHost(t *testing.T, dir string) string {
	t.Helper()
	return writeTopology(t, dir,
		hostSpec{
			id:       1,
			bindAddr: "10.0.0.1",
			addr:     "10.0.0.1",
			dataDir:  filepath.Join(dir, "data"),
			tlsCert:  filepath.Join(dir, "file.crt"),
			tlsKey:   filepath.Join(dir, "file.key"),
		},
		[]nodeSpec{
			{id: 1, role: "gate", port: 8443},
			{id: 2, role: "blob", port: 6660},
		})
}

// mergeHost loads the configuration at path and merges cfg's host-local fields
// into it, which is the pairing Start performs.
func mergeHost(t *testing.T, cfg *Config) (*pds.Config, error) {
	t.Helper()

	svc, err := New(cfg)
	require.NoError(t, err)

	parsed, err := pds.LoadConfig(cfg.ConfigPath)
	require.NoError(t, err)
	_, err = svc.mergeHost(parsed)
	return parsed, err
}

// gateOf is the gate node of the first host, which is where the merged S3 port
// lands.
func gateOf(t *testing.T, cfg *pds.Config) pds.NodeConfig {
	t.Helper()

	for _, n := range cfg.Hosts[0].Nodes {
		if n.Role == pds.RoleGate {
			return n
		}
	}
	t.Fatal("configuration declares no gate node")
	return pds.NodeConfig{}
}

// TestNew tests the service constructor.
func TestNew(t *testing.T) {
	cfg := &Config{
		ConfigPath:        "/tmp/test-config.toml",
		Port:              8443,
		Host:              "0.0.0.0",
		BasePath:          "/tmp/predastore",
		TlsCert:           "/tmp/cert.pem",
		TlsKey:            "/tmp/key.pem",
		EncryptionKeyFile: "/tmp/encryption.key",
		HostID:            2,
	}

	svc, err := New(cfg)

	require.NoError(t, err)
	require.NotNil(t, svc)
	// The same pointer, not a copy: the start command mutates the config it
	// hands over right up to the call.
	assert.Same(t, cfg, svc.Config)
}

// TestNewRejectsForeignConfig pins the type assertion in New: the service
// registry hands it an any, so a config meant for another service must be
// rejected rather than silently ignored.
func TestNewRejectsForeignConfig(t *testing.T) {
	svc, err := New(struct{ Port int }{Port: 8443})

	require.Error(t, err)
	assert.Nil(t, svc)
	assert.Contains(t, err.Error(), "invalid config type")
}

// TestServiceNameConstant tests the serviceName constant.
func TestServiceNameConstant(t *testing.T) {
	assert.Equal(t, "predastore", serviceName)
}

// TestReloadIsANoOp pins the current contract: predastore has no reload, and
// the service reports success rather than an error a caller would have to
// special-case.
func TestReloadIsANoOp(t *testing.T) {
	svc, err := New(&Config{})
	require.NoError(t, err)
	assert.NoError(t, svc.Reload())
}

// TestMergeHostFlagsBeatFile covers the precedence spinifex.toml relies on:
// the service flags own the host-local fields, so a value from them replaces
// whatever the predastore file carried.
func TestMergeHostFlagsBeatFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ConfigPath: singleHost(t, dir),
		Host:       "0.0.0.0",
		Port:       19443,
		TlsCert:    filepath.Join(dir, "flag.crt"),
		TlsKey:     filepath.Join(dir, "flag.key"),
		HostID:     1,
	}

	merged, err := mergeHost(t, cfg)

	require.NoError(t, err)
	assert.Equal(t, cfg.TlsCert, merged.Hosts[0].TLSCert)
	assert.Equal(t, cfg.TlsKey, merged.Hosts[0].TLSKey)
	assert.Equal(t, 19443, gateOf(t, merged).Port)
	// The dial address is the cluster's business, not this host's flags.
	assert.Equal(t, "10.0.0.1", merged.Hosts[0].Addr)
}

// TestMergeHostBindsTheGateNotTheCluster is the plane split: the address
// spinifex.toml carries is the public S3 endpoint, so settling it on the host
// would put raft and blob traffic on the public interface along with it.
func TestMergeHostBindsTheGateNotTheCluster(t *testing.T) {
	dir := t.TempDir()

	merged, err := mergeHost(t, &Config{
		ConfigPath: singleHost(t, dir),
		Host:       "0.0.0.0",
		HostID:     1,
	})

	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", gateOf(t, merged).BindAddr, "S3 answers on every interface")
	assert.Equal(t, "10.0.0.1", merged.Hosts[0].BindAddr, "raft and blob stay on the host address")
}

// TestMergeHostWithoutAnS3AddressLeavesBothPlanesOnTheHost covers the
// single-homed case: nothing to split, so the gate follows the host and the
// file alone decides.
func TestMergeHostWithoutAnS3AddressLeavesBothPlanesOnTheHost(t *testing.T) {
	dir := t.TempDir()

	merged, err := mergeHost(t, &Config{ConfigPath: singleHost(t, dir), HostID: 1})

	require.NoError(t, err)
	assert.Empty(t, gateOf(t, merged).BindAddr, "the gate names no address of its own")
	assert.Equal(t, "10.0.0.1", merged.Hosts[0].BindAddr)
}

// TestMergeHostReturnsTheResolvedHostID pins what Start hands predastore.Run:
// the host whose nodes this process serves, resolved once rather than
// reconverted at the call.
func TestMergeHostReturnsTheResolvedHostID(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(&Config{ConfigPath: singleHost(t, dir), HostID: 1})
	require.NoError(t, err)
	parsed, err := pds.LoadConfig(svc.Config.ConfigPath)
	require.NoError(t, err)

	hostID, err := svc.mergeHost(parsed)

	require.NoError(t, err)
	assert.Equal(t, pds.HostID(1), hostID)
}

// TestMergeHostKeepsFileValues is the other half of the precedence rule: an
// unset flag must leave the file's value alone rather than blanking it.
func TestMergeHostKeepsFileValues(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{ConfigPath: singleHost(t, dir), HostID: 1}

	merged, err := mergeHost(t, cfg)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", merged.Hosts[0].BindAddr)
	assert.Equal(t, filepath.Join(dir, "file.crt"), merged.Hosts[0].TLSCert)
	assert.Equal(t, filepath.Join(dir, "file.key"), merged.Hosts[0].TLSKey)
	assert.Equal(t, 8443, gateOf(t, merged).Port)
}

// TestMergeHostLeavesTheClusterPlaneToTheFile covers the one host-local field
// no service flag owns. There is no flag for the cluster plane, so a file that
// names only the dial address is left alone here and resolved by predastore,
// which is the single place that decides what an empty bind address means.
func TestMergeHostLeavesTheClusterPlaneToTheFile(t *testing.T) {
	dir := t.TempDir()
	path := writeTopology(t, dir,
		hostSpec{id: 1, addr: "10.0.0.1", dataDir: filepath.Join(dir, "data")},
		[]nodeSpec{{id: 1, role: "gate", port: 8443}, {id: 2, role: "blob", port: 6660}})

	merged, err := mergeHost(t, &Config{ConfigPath: path, Host: "0.0.0.0", HostID: 1})

	require.NoError(t, err)
	assert.Empty(t, merged.Hosts[0].BindAddr, "the S3 address must not settle on the host")
	assert.Equal(t, "10.0.0.1", pds.HostBindAddr(merged.Hosts[0]), "predastore resolves it to the dial address")
}

// TestMergeHostRejectsMissingHostID covers the one field with no default: the
// host id selects which [[host]] this process runs, and zero names none.
func TestMergeHostRejectsMissingHostID(t *testing.T) {
	dir := t.TempDir()

	_, err := mergeHost(t, &Config{ConfigPath: singleHost(t, dir)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "host id is required")
}

// TestMergeHostRejectsUnknownHostID covers a host id the configuration file
// never declares: nothing pins nodes to it, so the process would serve an
// empty cluster.
func TestMergeHostRejectsUnknownHostID(t *testing.T) {
	dir := t.TempDir()

	_, err := mergeHost(t, &Config{ConfigPath: singleHost(t, dir), HostID: 9})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "predastore host 9 is not in")
}

// TestMergeHostRejectsHostWithoutGate covers a host running no gate: the
// endpoint spinifex.toml advertises for this node would answer no S3 request,
// and the port the flags carry would have nowhere to land.
func TestMergeHostRejectsHostWithoutGate(t *testing.T) {
	dir := t.TempDir()
	path := writeTopology(t, dir,
		hostSpec{id: 1, addr: "10.0.0.1", dataDir: filepath.Join(dir, "data")},
		[]nodeSpec{{id: 1, role: "blob", port: 6660}, {id: 2, role: "meta", port: 7660}})

	_, err := mergeHost(t, &Config{ConfigPath: path, HostID: 1, Port: 18443})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs no gate node")
}

// TestMergeHostRevalidatesMergedConfig is why the merge ends in Validate: a
// port that only arrives by flag can still collide with a sibling node's, and
// the file alone was coherent.
func TestMergeHostRevalidatesMergedConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{ConfigPath: singleHost(t, dir), HostID: 1, Port: 6660}

	_, err := mergeHost(t, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "both use port 6660")
}

// TestStartRejectsMissingEncryptionKey ensures Start fails fast when
// EncryptionKeyFile is unset, before touching the pid file or the config.
func TestStartRejectsMissingEncryptionKey(t *testing.T) {
	svc, err := New(&Config{
		ConfigPath: filepath.Join(t.TempDir(), "predastore.toml"),
		BasePath:   t.TempDir(),
		HostID:     1,
	})
	require.NoError(t, err)

	pid, err := svc.Start()

	require.Error(t, err)
	assert.Zero(t, pid)
	assert.Contains(t, err.Error(), "encryption key file is required")
}

// TestStatusReportsStoppedWithoutPidFile covers the resting state: no pid file
// under BasePath means nothing is running, not an error.
func TestStatusReportsStoppedWithoutPidFile(t *testing.T) {
	svc, err := New(&Config{BasePath: t.TempDir()})
	require.NoError(t, err)

	status, err := svc.Status()

	require.NoError(t, err)
	assert.Equal(t, "stopped", status)
}

// TestStatusAndStopUseThePidFile covers the out-of-process path: Stop and
// Status find a predastore started by an earlier invocation through the pid
// file BasePath holds, and Stop clears it once the process is gone.
func TestStatusAndStopUseThePidFile(t *testing.T) {
	basePath := t.TempDir()
	svc, err := New(&Config{BasePath: basePath})
	require.NoError(t, err)

	child := exec.CommandContext(t.Context(), "sleep", "60")
	require.NoError(t, child.Start())
	// Reaped as it dies, not at cleanup: an unwaited child of this process
	// lingers as a zombie, which still answers signal 0, so Stop would wait out
	// its whole grace period before escalating to SIGKILL.
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		_ = child.Wait()
	}()
	t.Cleanup(func() { <-reaped })
	require.NoError(t, utils.WritePidFileTo(basePath, serviceName, child.Process.Pid))

	status, err := svc.Status()
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("running (pid: %d)", child.Process.Pid), status)

	require.NoError(t, svc.Stop())
	assert.NoFileExists(t, filepath.Join(basePath, serviceName+".pid"))

	status, err = svc.Status()
	require.NoError(t, err)
	assert.Equal(t, "stopped", status)
}

// TestShutdownWithoutRunningClusterFallsBackToPidFile covers the fallback: with
// no cluster in this process there is no context to cancel, so Shutdown goes
// through the pid file, which reports rather than signalling anything.
func TestShutdownWithoutRunningClusterFallsBackToPidFile(t *testing.T) {
	svc, err := New(&Config{BasePath: t.TempDir()})
	require.NoError(t, err)

	assert.Error(t, svc.Shutdown())
}

// TestShutdownCancelsTheServiceContext covers the in-process path: once Start
// has published its cancel func, Shutdown cancels the context the gate and the
// local nodes share instead of signalling through the pid file.
func TestShutdownCancelsTheServiceContext(t *testing.T) {
	svc, err := New(&Config{BasePath: t.TempDir()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.mu.Lock()
	svc.stop = cancel
	svc.mu.Unlock()

	require.NoError(t, svc.Shutdown())
	assert.Error(t, ctx.Err())
}
