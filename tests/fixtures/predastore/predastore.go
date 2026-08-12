// Package predastore starts a real predastore cluster for tests that need to
// exercise an actual S3-compatible backend rather than a mock. It is
// deliberately its own leaf package (not folded into spinifex/testutil,
// which is imported by most of the module's test files for lightweight NATS
// helpers): starting one here runs a whole predastore host — blob nodes, Raft
// meta replicas and the S3 gate in front of them — whose goroutines run for
// the life of the test binary. Folding this file into the shared testutil
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
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/bluebottle/pkg/masterkey"
	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/tests/fixtures/scratch"
)

// Fixed connection details for the shared predastore fixture started by
// Start. Every cluster node runs in-process, so these values never need to
// vary per caller or per test run.
const (
	Host   = "127.0.0.1:18443"
	Region = "us-east-1"
	// AccessKey/SecretKey are the well-known AWS SDK example credentials
	// (docs.aws.amazon.com/IAM/latest/UserGuide), used only to authenticate
	// against this ephemeral, localhost-only test cluster — not a real secret.
	AccessKey = "AKIAIOSFODNN7EXAMPLE"                     //nolint:gosec // well-known AWS SDK example key, test-only cluster
	SecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" //nolint:gosec // well-known AWS SDK example secret, test-only cluster

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

// The topology Start writes. One host runs every node, so they talk over the
// in-process pipe and only the gate binds a socket. RS(3,2) spreads a stripe
// over 5 distinct blob nodes, which is exactly how many there are — so every
// PutObject lands one shard on each — and 3 meta replicas form the quorum.
const (
	fixtureHostID = 1
	rsData        = 3
	rsParity      = 2

	gateNodeID    = 1
	firstBlobNode = 2
	blobNodes     = rsData + rsParity
	firstMetaNode = firstBlobNode + blobNodes
	metaNodes     = 3

	// Cluster ports are never dialled — the pipe transport keys its registry
	// by them, nothing binds them — but each must be unique within the host.
	firstBlobPort = 16660
	firstMetaPort = 17660

	// accountID owns every bucket the fixture credentials create.
	accountID = "123456789012"
)

// Fixture describes a running predastore cluster ready for real
// S3/viperblock clients: a reachable endpoint and its default buckets
// created. Its TLS cert is self-signed, so clients skip verification.
type Fixture struct {
	Host      string
	Region    string
	AccessKey string
	SecretKey string
	// DataDir is the host's data root: it contains node-N/ for each node that
	// keeps state (db/, *.seg, *.idx), the exact layout a segment-inspection
	// tool like segscan expects. The cluster keeps writing here for the life
	// of the test binary, so callers that read it directly (rather than
	// through the S3 API) must copy it first.
	DataDir string
}

// Package-level singleton: one predastore cluster per test binary (per Go
// package under test), amortising its startup cost across every test that
// calls Start instead of paying it per-test. Guarded by a mutex rather than
// sync.Once because a failed first attempt should not wedge every later
// caller into replaying a t.Fatalf from a different test's *testing.T.
var (
	mu      sync.Mutex
	started bool
	fixture *Fixture
	stop    context.CancelFunc
	drained chan struct{}
)

// fixtureDirPrefix names each run's fixture directory. It is a constant
// because the sweep in Start matches on it.
const fixtureDirPrefix = "predastore-fixture-"

// Start starts a real predastore cluster the first time it's called within a
// test binary and returns connection details for it; every later call in
// the same process returns the already-running fixture. The cluster
// deliberately outlives any individual test — its temp dir uses
// os.MkdirTemp rather than t.TempDir() so a finished test's cleanup can't
// delete files the shared cluster still has open — and runs until Stop, or
// until the test process exits.
func Start(t *testing.T) *Fixture {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()

	if started {
		return fixture
	}

	// The cluster can outlive this run, so there is no point at which it can
	// remove its own directory. Reclaiming it is left to a later run's sweep,
	// which is also what recovers a run killed mid-flight.
	scratch.SweepAbandoned(os.TempDir(), fixtureDirPrefix, scratch.DefaultMaxAge)

	testDir, err := os.MkdirTemp("", fixtureDirPrefix+"*") //nolint:usetesting // shared cluster outlives individual tests
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
	// for one). t.Setenv reverts when this test ends, while the cluster lives
	// on for the whole binary, so the pool is loaded here and now: crypto/x509
	// caches it behind a sync.Once, and this call is what fixes our cert in it
	// for every later test in the process.
	t.Setenv("SSL_CERT_FILE", certPath)
	if _, err := x509.SystemCertPool(); err != nil {
		t.Fatalf("predastore fixture: load system cert pool: %v", err)
	}

	// Predastore mandates a 32-byte master key at mode 0600 (masterkey.Load is
	// fail-closed on anything group- or other-readable).
	encryptionKeyPath := filepath.Join(testDir, "encryption.key")
	testEncryptionKey := make([]byte, 32)
	if _, err := rand.Read(testEncryptionKey); err != nil {
		t.Fatalf("predastore fixture: generate encryption key: %v", err)
	}
	if err := os.WriteFile(encryptionKeyPath, testEncryptionKey, 0600); err != nil {
		t.Fatalf("predastore fixture: write encryption key: %v", err)
	}

	// The config goes through the real file and the real loader rather than a
	// hand-built struct, so the fixture trips over the same strict decode and
	// topology validation an operator's install would.
	dataDir := filepath.Join(testDir, "data")
	configPath := filepath.Join(testDir, "predastore.toml")
	config := topology(dataDir, certPath, keyPath, encryptionKeyPath)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("predastore fixture: write config: %v", err)
	}
	cfg, err := pds.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("predastore fixture: load config: %v", err)
	}

	key, err := masterkey.Load(encryptionKeyPath)
	if err != nil {
		t.Fatalf("predastore fixture: load master key: %v", err)
	}

	// Run blocks for as long as it serves and drains everything it started on
	// the way out, so the cancel and the done channel are the whole handle
	// Stop needs. A t.Fatalf below takes the binary down with it, so nothing
	// is left orphaned either way.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := pds.Run(ctx, pds.Options{
			Config:    cfg,
			HostID:    pds.HostID(fixtureHostID),
			MasterKey: key,
		}); err != nil && ctx.Err() == nil {
			slog.Error("predastore fixture: cluster exited", "error", err)
		}
	}()
	stop, drained = cancel, done

	// The gate holds off serving until the local Raft quorum has a leader, so
	// an accepted connection also means writes will commit.
	if !waitForReady(30*time.Second, done) {
		cancel()
		t.Fatal("predastore fixture: S3 gate did not become ready")
	}

	// Create the default buckets through the S3 API so they land in the meta
	// plane; config-defined buckets are not visible to ListBuckets.
	setupClient := s3Client(AccessKey, SecretKey)
	for _, bucket := range []string{DefaultBucket, DefaultBucket2} {
		if _, err := setupClient.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			cancel()
			t.Fatalf("predastore fixture: create bucket %s: %v", bucket, err)
		}
	}

	fixture = &Fixture{
		Host:      Host,
		Region:    Region,
		AccessKey: AccessKey,
		SecretKey: SecretKey,
		DataDir:   dataDir,
	}
	started = true
	t.Logf("predastore fixture started, test dir: %s", testDir)

	return fixture
}

// Stop cancels the cluster and waits for it to drain. It is for a TestMain
// that wants the goroutines gone before the binary reports; tests themselves
// share the one cluster and must not stop it. Calling it without a running
// fixture, or twice, is a no-op.
func Stop() {
	mu.Lock()
	defer mu.Unlock()

	if !started {
		return
	}
	stop()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		slog.Error("predastore fixture: cluster did not drain")
	}
	started, fixture = false, nil
}

// topology is the fixture's predastore configuration file: one host, the gate
// on the fixed S3 port, and the blob and meta nodes the erasure code needs.
func topology(dataDir, certPath, keyPath, encryptionKeyPath string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "version = 1\nregion = %q\n\n[rs]\ndata = %d\nparity = %d\n\n", Region, rsData, rsParity)
	fmt.Fprintf(&b, "[[host]]\nid = %d\nbind_addr = %q\naddr = %q\ndata_dir = %q\ntls_cert = %q\ntls_key = %q\nencryption_key = %q\n\n",
		fixtureHostID, testHost, testHost, dataDir, certPath, keyPath, encryptionKeyPath)

	fmt.Fprintf(&b, "[[host.node]]\nid = %d\nrole = \"gate\"\nport = %d\n\n", gateNodeID, testPort)
	for i := range blobNodes {
		fmt.Fprintf(&b, "[[host.node]]\nid = %d\nrole = \"blob\"\nport = %d\n\n", firstBlobNode+i, firstBlobPort+i)
	}
	for i := range metaNodes {
		fmt.Fprintf(&b, "[[host.node]]\nid = %d\nrole = \"meta\"\nport = %d\n\n", firstMetaNode+i, firstMetaPort+i)
	}

	// No policy: a config-defined account is a trusted service account, so the
	// gate skips the policy check for it entirely.
	fmt.Fprintf(&b, "[[auth]]\naccess_key_id = %q\nsecret_access_key = %q\naccount_id = %q\n",
		AccessKey, SecretKey, accountID)

	return b.String()
}

// generateCertificate writes a self-signed TLS certificate and key for the
// fixture's S3 HTTPS frontend. Clients reach it with InsecureSkipVerify, so
// nothing needs the cert in a trust store.
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

// waitForReady polls the S3 endpoint until it answers, the cluster exits or
// timeout elapses. A cluster that failed to start closes done, which is the
// difference between waiting out the timeout and reporting at once.
func waitForReady(timeout time.Duration, done <-chan struct{}) bool {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed cert generated by this same fixture, localhost-only
		},
		Timeout: 1 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false
		default:
		}
		resp, err := client.Get("https://" + net.JoinHostPort(testHost, strconv.Itoa(testPort)) + "/")
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// s3Client creates an AWS S3 client against the fixture for fixture setup
// only (bucket creation); test bodies build their own clients against
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
