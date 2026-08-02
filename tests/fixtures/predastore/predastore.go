// Package predastore starts a real predastore daemon for tests that need to
// exercise an actual S3-compatible backend rather than a mock. It is
// deliberately its own leaf package (not folded into spinifex/testutil,
// which is imported by most of the module's test files for lightweight NATS
// helpers): starting a daemon here builds a whole predastore cluster runtime
// — shard stores, Raft replicas and their transports — whose goroutines run
// for the life of the test binary. Folding this file into the shared testutil
// package would pull all of that into every test binary that imports
// testutil, and trip up any unrelated goroutine-leak check (go.uber.org/goleak)
// running in that same binary. Only callers that actually need a real
// predastore should pay for it.
package predastore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/predastore/clusterrun"
	"github.com/mulgadc/predastore/pkg/masterkey"
	predastoreserver "github.com/mulgadc/predastore/s3"
	"github.com/mulgadc/spinifex/tests/fixtures/scratch"
)

// Fixed connection details for the shared predastore fixture daemon started
// by Start. Every cluster node runs in-process, so these values never need to
// vary per caller or per test run.
const (
	Host   = "127.0.0.1:18443"
	Region = "us-east-1"
	// AccessKey/SecretKey are the well-known AWS SDK example credentials
	// (docs.aws.amazon.com/IAM/latest/UserGuide), used only to authenticate
	// against this ephemeral, localhost-only test daemon — not a real secret.
	AccessKey = "AKIAIOSFODNN7EXAMPLE"                     //nolint:gosec // well-known AWS SDK example key, test-only daemon
	SecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" //nolint:gosec // well-known AWS SDK example secret, test-only daemon

	// DefaultBucket and DefaultBucket2 are pre-created by Start itself,
	// matching the bucket set services/predastore's own integration tests
	// were written against. Callers that need a different bucket should
	// EnsureBucket one of their own against the fixture (see
	// objectstore.NewS3ObjectStoreFromConfig) rather than reuse these —
	// they're an implementation detail of this package's own tests, not a
	// general-purpose bucket.
	DefaultBucket  = "test-bucket"
	DefaultBucket2 = "public-bucket"

	testHost = "127.0.0.1"
	testPort = 18443
)

// Fixture describes a running predastore daemon ready for real
// S3/viperblock clients: a reachable endpoint and its default buckets
// created. Its TLS cert is self-signed, so clients skip verification.
type Fixture struct {
	Host      string
	Region    string
	AccessKey string
	SecretKey string
	// DataDir is the daemon's base_path: it contains store/node-N/ for each
	// configured node (db/, *.seg, *.idx, state.json), the exact layout a
	// segment-inspection tool like segscan expects. The daemon keeps writing
	// here for the life of the test binary, so callers that read it directly
	// (rather than through the S3 API) must copy it first.
	DataDir string
}

// Package-level singleton: one predastore daemon per test binary (per Go
// package under test), amortising its startup cost across every test that
// calls Start instead of paying it per-test. Guarded by a mutex rather than
// sync.Once because a failed first attempt should not wedge every later
// caller into replaying a t.Fatalf from a different test's *testing.T.
var (
	mu      sync.Mutex
	started bool
	fixture *Fixture
)

// fixtureDirPrefix names each run's fixture directory. It is a constant
// because the sweep in Start matches on it.
const fixtureDirPrefix = "predastore-fixture-"

// Start starts a real predastore daemon the first time it's called within a
// test binary and returns connection details for it; every later call in
// the same process returns the already-running fixture. The daemon
// deliberately outlives any individual test — its temp dir uses
// os.MkdirTemp rather than t.TempDir() so a finished test's cleanup can't
// delete files the shared daemon still has open — and is left running until
// the test process exits.
func Start(t *testing.T) *Fixture {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()

	if started {
		return fixture
	}

	// The daemon runs until the process exits, so there is no point at which
	// this run can remove its own directory. Reclaiming it is left to a later
	// run's sweep, which is also what recovers a run killed mid-flight.
	scratch.SweepAbandoned(os.TempDir(), fixtureDirPrefix, scratch.DefaultMaxAge)

	testDir, err := os.MkdirTemp("", fixtureDirPrefix+"*") //nolint:usetesting // shared daemon outlives individual tests
	if err != nil {
		t.Fatalf("predastore fixture: create temp dir: %v", err)
	}

	certDir := filepath.Join(testDir, "certs")
	if err := os.MkdirAll(certDir, 0755); err != nil { //nolint:gosec // ephemeral test-only temp dir, not sensitive
		t.Fatalf("predastore fixture: create cert dir: %v", err)
	}
	certPath := filepath.Join(certDir, "test.crt")
	keyPath := filepath.Join(certDir, "test.key")

	// The cert serves the S3 HTTPS frontend only. Intra-cluster traffic never
	// leaves the process here, so it needs no TLS material of its own.
	if err := generateCertificate(certPath, keyPath); err != nil {
		t.Fatalf("predastore fixture: generate certificate: %v", err)
	}

	// SSL_CERT_FILE injects the cert into the OS trust store for clients that
	// verify it rather than skipping verification (objectstore's S3 client,
	// for one). t.Setenv reverts when this test ends, while the daemon lives
	// on for the whole binary, so the pool is loaded here and now: crypto/x509
	// caches it behind a sync.Once, and this call is what fixes our cert in it
	// for every later test in the process.
	t.Setenv("SSL_CERT_FILE", certPath)
	if _, err := x509.SystemCertPool(); err != nil {
		t.Fatalf("predastore fixture: load system cert pool: %v", err)
	}

	// Predastore mandates a 32-byte master key at mode 0600 (rejected
	// otherwise by internal/keyfile.Load).
	encryptionKeyPath := filepath.Join(testDir, "encryption.key")
	testEncryptionKey := make([]byte, 32)
	if _, err := rand.Read(testEncryptionKey); err != nil {
		t.Fatalf("predastore fixture: generate encryption key: %v", err)
	}
	if err := os.WriteFile(encryptionKeyPath, testEncryptionKey, 0600); err != nil {
		t.Fatalf("predastore fixture: write encryption key: %v", err)
	}

	// One host carrying every node, so the whole cluster runs in this process
	// over the in-process pipe: no sockets to bind, no intra-cluster certs.
	// RS(3,2) needs 5 shard-storage nodes; 3 state replicas form the quorum.
	configPath := filepath.Join(testDir, "predastore_test.toml")
	configContent := `version = "1.0"
region = "us-east-1"
debug = false
disable_logging = false
base_path = "` + testDir + `/"

[rs]
data = 3
parity = 2

[[host]]
id = 1
bind_addr = "127.0.0.1:16660"
public_addr = "127.0.0.1:16660"
data_dir = "` + filepath.Join(testDir, "cluster") + `"

[[node]]
id = 1
host_id = 1
role = "shard-storage"

[[node]]
id = 2
host_id = 1
role = "shard-storage"

[[node]]
id = 3
host_id = 1
role = "shard-storage"

[[node]]
id = 4
host_id = 1
role = "shard-storage"

[[node]]
id = 5
host_id = 1
role = "shard-storage"

[[node]]
id = 6
host_id = 1
role = "state-replica"

[[node]]
id = 7
host_id = 1
role = "state-replica"

[[node]]
id = 8
host_id = 1
role = "state-replica"

[[auth]]
access_key_id = "` + AccessKey + `"
secret_access_key = "` + SecretKey + `"
account_id = "123456789012"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil { //nolint:gosec // ephemeral test-only config, contains no real secrets
		t.Fatalf("predastore fixture: write config: %v", err)
	}

	// The S3 gateway runs on top of a backend the caller wires: the cluster
	// runtime owns the shard stores and the Raft replicas, and this process
	// runs all of them.
	cfg := &predastoreserver.Config{ConfigPath: configPath, BasePath: testDir}
	if err := cfg.ReadConfig(); err != nil {
		t.Fatalf("predastore fixture: read config: %v", err)
	}
	key, err := masterkey.Load(encryptionKeyPath)
	if err != nil {
		t.Fatalf("predastore fixture: load master key: %v", err)
	}
	rt, err := clusterrun.Build(cfg, clusterrun.AllNodeIDs(cfg), certPath, keyPath, key)
	if err != nil {
		t.Fatalf("predastore fixture: build cluster runtime: %v", err)
	}

	// Built directly against predastore/s3 rather than spinifex's own
	// services/predastore wrapper: that wrapper pulls in spinifex/utils,
	// and spinifex/utils' own tests import spinifex/testutil, which would be
	// an import cycle if this package were reached from there. The
	// wrapper's only other job — pidfile bookkeeping and signal-triggered
	// shutdown — is irrelevant for a fixture daemon that lives for the
	// whole test binary anyway.
	server, err := predastoreserver.NewServer(
		predastoreserver.WithConfigPath(configPath),
		predastoreserver.WithAddress(testHost, testPort),
		predastoreserver.WithTLS(certPath, keyPath),
		predastoreserver.WithBasePath(testDir),
		predastoreserver.WithDebug(false),
		predastoreserver.WithPprof(false, ""),
		predastoreserver.WithEncryptionKeyFile(encryptionKeyPath),
		predastoreserver.WithPreparedBackend(rt.Backend),
	)
	if err != nil {
		t.Fatalf("predastore fixture: create server: %v", err)
	}

	// The runtime is as permanent as the daemon it backs: an uncancellable
	// context, left running until the test process exits. A t.Fatalf below
	// takes the binary down with it, so nothing is left orphaned.
	go func() {
		if err := rt.Run(context.Background()); err != nil {
			slog.Error("predastore fixture: cluster runtime exited", "error", err)
		}
	}()

	// Writes need a committed leader; serving before one exists would fail
	// the bucket creation below for no reason other than timing.
	if err := rt.WaitReady(30 * time.Second); err != nil {
		t.Fatalf("predastore fixture: no leader elected: %v", err)
	}

	if err := server.ListenAndServeAsync(); err != nil {
		t.Fatalf("predastore fixture: start server: %v", err)
	}

	if !waitForReady(10 * time.Second) {
		t.Fatal("predastore fixture: server did not become ready")
	}

	// Create the default buckets via the S3 API so they're registered in
	// distributed globalState (config buckets aren't visible to ListBuckets).
	setupClient := s3Client(AccessKey, SecretKey)
	for _, bucket := range []string{DefaultBucket, DefaultBucket2} {
		if _, err := setupClient.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("predastore fixture: create bucket %s: %v", bucket, err)
		}
	}

	fixture = &Fixture{
		Host:      Host,
		Region:    Region,
		AccessKey: AccessKey,
		SecretKey: SecretKey,
		DataDir:   testDir,
	}
	started = true
	t.Logf("predastore fixture started, test dir: %s", testDir)

	return fixture
}

// generateCertificate writes a self-signed TLS certificate and key for the
// fixture daemon's S3 HTTPS frontend. Clients reach it with
// InsecureSkipVerify, so nothing needs the cert in a trust store.
func generateCertificate(certPath, keyPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(24 * time.Hour)

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Predastore Test"},
			CommonName:   "localhost",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return err
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}); err != nil {
		return err
	}

	return nil
}

// waitForReady polls the fixture daemon's HTTPS endpoint until it accepts
// connections or timeout elapses.
func waitForReady(timeout time.Duration) bool {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert generated by this same fixture, localhost-only
		},
		Timeout: 1 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + net.JoinHostPort(testHost, strconv.Itoa(testPort)) + "/")
		if err == nil {
			resp.Body.Close()
			// Give a bit more time for config to load.
			time.Sleep(500 * time.Millisecond)
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// s3Client creates an AWS S3 client against the fixture daemon for fixture
// setup only (bucket creation); test bodies build their own clients against
// whatever bucket/credentials their scenario needs.
func s3Client(accessKey, secretKey string) *s3.S3 {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert generated by this same fixture, localhost-only
	}
	httpClient := &http.Client{Transport: tr}

	sess := session.Must(session.NewSession(&aws.Config{
		Region:           aws.String(Region),
		Endpoint:         aws.String("https://" + Host),
		Credentials:      credentials.NewStaticCredentials(accessKey, secretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(false),
		HTTPClient:       httpClient,
	}))

	return s3.New(sess)
}
