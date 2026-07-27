// Package predastore starts a real predastore daemon for tests that need to
// exercise an actual S3-compatible backend rather than a mock. It is
// deliberately its own leaf package (not folded into spinifex/testutil,
// which is imported by most of the module's test files for lightweight NATS
// helpers): this package transitively imports predastore/quic/quicclient,
// whose package-level DefaultPool starts a permanent cleanup goroutine at
// init time. Folding this file into the shared testutil package would pull
// that goroutine into every test binary that imports testutil, and trip up
// any unrelated goroutine-leak check (go.uber.org/goleak) running in that
// same binary. Only callers that actually need a real predastore should pay
// for this import.
package predastore

import (
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
	"github.com/mulgadc/predastore/quic/quicclient"
	predastoreserver "github.com/mulgadc/predastore/s3"
)

// Fixed connection details for the shared predastore fixture daemon started
// by Start. NodeID -1 (dev mode) runs every QUIC shard node in-process, so
// these values never need to vary per caller or per test run.
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
// S3/viperblock clients: a reachable endpoint, TLS material already trusted
// process-wide, and its default buckets created.
type Fixture struct {
	Host      string
	Region    string
	AccessKey string
	SecretKey string
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

	testDir, err := os.MkdirTemp("", "predastore-fixture-*") //nolint:usetesting // shared daemon outlives individual tests
	if err != nil {
		t.Fatalf("predastore fixture: create temp dir: %v", err)
	}

	certDir := filepath.Join(testDir, "certs")
	if err := os.MkdirAll(certDir, 0755); err != nil { //nolint:gosec // ephemeral test-only temp dir, not sensitive
		t.Fatalf("predastore fixture: create cert dir: %v", err)
	}
	certPath := filepath.Join(certDir, "test.crt")
	keyPath := filepath.Join(certDir, "test.key")

	caPool, err := generateCertificate(certPath, keyPath)
	if err != nil {
		t.Fatalf("predastore fixture: generate certificate: %v", err)
	}

	// Inject the ephemeral cert for the QUIC client (s3d -> shard nodes).
	quicclient.SetDefaultRootCAs(caPool)

	// SSL_CERT_FILE injects the cert for the s3db REST client's OS trust
	// store (sync.Once-cached there, so it must be set before that client's
	// first dial). Scoped to this call via t.Setenv, but since the daemon and
	// its first client dial both happen inside this same call, the cache is
	// warm before this function returns — later tests in the process don't
	// need the env var set again.
	t.Setenv("SSL_CERT_FILE", certPath)

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

	// Five nodes trigger dev-mode: all QUIC shards start as local goroutines.
	configPath := filepath.Join(testDir, "predastore_test.toml")
	configContent := `version = "1.0"
region = "us-east-1"
host = "127.0.0.1"
port = 18443
debug = false
disable_logging = false
base_path = "` + testDir + `/"

[rs]
data = 3
parity = 2

[[nodes]]
id = 1
host = "127.0.0.1"
port = 19991
path = "store/node-1/"

[[nodes]]
id = 2
host = "127.0.0.1"
port = 19992
path = "store/node-2/"

[[nodes]]
id = 3
host = "127.0.0.1"
port = 19993
path = "store/node-3/"

[[nodes]]
id = 4
host = "127.0.0.1"
port = 19994
path = "store/node-4/"

[[nodes]]
id = 5
host = "127.0.0.1"
port = 19995
path = "store/node-5/"

[[auth]]
access_key_id = "` + AccessKey + `"
secret_access_key = "` + SecretKey + `"
account_id = "123456789012"
`

	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil { //nolint:gosec // ephemeral test-only config, contains no real secrets
		t.Fatalf("predastore fixture: write config: %v", err)
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
		predastoreserver.WithNodeID(-1),
		predastoreserver.WithPprof(false, ""),
		predastoreserver.WithEncryptionKeyFile(encryptionKeyPath),
	)
	if err != nil {
		t.Fatalf("predastore fixture: create server: %v", err)
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
	}
	started = true
	t.Logf("predastore fixture started, test dir: %s", testDir)

	return fixture
}

// generateCertificate generates a self-signed TLS certificate for the
// fixture daemon and returns a CertPool containing it, for injection into
// whichever client trust store a caller needs (see quicclient.SetDefaultRootCAs
// and the SSL_CERT_FILE handling in Start).
func generateCertificate(certPath, keyPath string) (*x509.CertPool, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(24 * time.Hour)

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
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
		return nil, err
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return nil, err
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return nil, err
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return nil, err
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}); err != nil {
		return nil, err
	}

	parsed, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return pool, nil
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
