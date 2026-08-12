package predastore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cluster these tests start binds 127.0.0.2 so nothing it registers — the
// in-process pipe transport keys its listeners by address and port, for the
// life of the binary — can collide with the shared fixture on 127.0.0.1.
const (
	startAddr     = "127.0.0.2"
	startBlobPort = 26660
	startMetaPort = 27660
)

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

// writeCertificate writes a self-signed P-256 certificate for the S3 gate.
// Nothing verifies it: the only client is this test's own dialer.
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
		IPAddresses:           []net.IP{net.ParseIP(startAddr)},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "tls.crt")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))

	der, err = x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPath = filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600))

	return certPath, keyPath
}

// freePort reserves and releases a port on the test address, so the gate binds
// somewhere no other test in this binary has taken.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(startAddr, "0"))
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// servableHost writes the smallest topology a host can actually serve: a gate
// for the S3 API, the one blob node RS(1,0) needs, and a meta replica, since
// the gate reaches the metadata plane through at least one.
func servableHost(t *testing.T, dir string, gatePort int) string {
	t.Helper()

	certPath, keyPath := writeCertificate(t, dir)
	return writeTopology(t, dir,
		hostSpec{
			id:      1,
			addr:    startAddr,
			dataDir: filepath.Join(dir, "data"),
			tlsCert: certPath,
			tlsKey:  keyPath,
		},
		[]nodeSpec{
			{id: 1, role: "gate", port: gatePort},
			{id: 2, role: "blob", port: startBlobPort},
			{id: 3, role: "meta", port: startMetaPort},
		})
}

// TestStartRejectsUnreadableConfig covers the first thing Start reads after the
// pid file: a configuration path that names no file stops the service before
// any node is built.
func TestStartRejectsUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(&Config{
		ConfigPath:        filepath.Join(dir, "absent.toml"),
		BasePath:          dir,
		EncryptionKeyFile: writeMasterKey(t, dir, 0600),
		HostID:            1,
	})
	require.NoError(t, err)

	pid, err := svc.Start()

	require.Error(t, err)
	assert.Zero(t, pid)
	assert.Contains(t, err.Error(), "read predastore config")
}

// TestStartRejectsUnknownHost covers the merge failing inside Start: the error
// travels out unwrapped, because it already names the host and the file.
func TestStartRejectsUnknownHost(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(&Config{
		ConfigPath:        singleHost(t, dir),
		BasePath:          dir,
		EncryptionKeyFile: writeMasterKey(t, dir, 0600),
		HostID:            9,
	})
	require.NoError(t, err)

	pid, err := svc.Start()

	require.Error(t, err)
	assert.Zero(t, pid)
	assert.Contains(t, err.Error(), "predastore host 9 is not in")
}

// TestStartRejectsUnprotectedMasterKey covers the at-rest key: predastore
// refuses one any other account can read, and the service must surface that
// rather than start without encryption.
func TestStartRejectsUnprotectedMasterKey(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(&Config{
		ConfigPath:        singleHost(t, dir),
		BasePath:          dir,
		EncryptionKeyFile: writeMasterKey(t, dir, 0644),
		HostID:            1,
	})
	require.NoError(t, err)

	pid, err := svc.Start()

	require.Error(t, err)
	assert.Zero(t, pid)
	assert.Contains(t, err.Error(), "load predastore master key")
}

// TestStartServesUntilShutdown covers the service lifecycle end to end: Start
// blocks for as long as the host serves, and Shutdown cancels the context the
// gate and the local nodes share, which is what lets Start return at all.
func TestStartServesUntilShutdown(t *testing.T) {
	dir := t.TempDir()
	basePath := t.TempDir()
	gatePort := freePort(t)
	svc, err := New(&Config{
		ConfigPath:        servableHost(t, dir, gatePort),
		BasePath:          basePath,
		EncryptionKeyFile: writeMasterKey(t, dir, 0600),
		HostID:            1,
	})
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

	// Shutdown must not run before Start has published its cancel func, and an
	// accepted connection is the first observable point at which it has.
	addr := net.JoinHostPort(startAddr, strconv.Itoa(gatePort))
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 60*time.Second, 100*time.Millisecond, "gate never accepted a connection on %s", addr)

	assert.FileExists(t, filepath.Join(basePath, serviceName+".pid"))
	require.NoError(t, svc.Shutdown())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, os.Getpid(), res.pid)
	case <-time.After(60 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}
