package workerboot

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
)

// storeIdentity is a store guest's self-signed TLS server key and its pin.
type storeIdentity struct {
	cert tls.Certificate
	pin  string
}

func newStoreIdentity() storeIdentity {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sqs-store"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return storeIdentity{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pin: sqsv1.Pin(leaf)}
}

// testStore is the identity every test store serves unless a test names
// another; testStorePin is what the gateway records for it.
var testStore = newStoreIdentity()

var testStorePin = testStore.pin

// tlsStore is a real NATS server with JetStream that requires TLS 1.3 with
// ident's key and admits only pub. Port 0 picks a free port.
func tlsStore(t *testing.T, pub string, port int, ident storeIdentity) *server.Server {
	t.Helper()
	if port == 0 {
		port = -1
	}
	opts := &server.Options{
		Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
		Nkeys:     []*server.NkeyUser{{Nkey: pub}},
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{ident.cert}},
		TLS:       true, TLSTimeout: 2,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("store server did not start")
	}
	t.Cleanup(ns.Shutdown)
	return ns
}

// The checker and connector send the worker key only to the pinned store:
// another store's key, a plaintext store or a malformed pin is refused as
// final, and the wrong store is left holding no client.
func TestPinnedTLS_RefusesAnyStoreButThePinnedOne(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	wrong := tlsStore(t, pub, 0, newStoreIdentity())
	port := wrong.Addr().(*net.TCPAddr).Port
	c := NATSChecker{Port: port, Dial: 2 * time.Second}
	err := c.Check(context.Background(), "127.0.0.1", testStorePin, kp)
	if !errors.Is(err, ErrStoreIdentity) || retryable(err) {
		t.Fatalf("wrong store: %v", err)
	}
	if _, _, err := (NATSConnector{Port: port}).Connect(context.Background(), "127.0.0.1", testStorePin, kp); !errors.Is(err, ErrStoreIdentity) || retryable(err) {
		t.Fatalf("connector, wrong store: %v", err)
	}
	// The server counts a client from TCP accept; the refused one must go.
	for deadline := time.Now().Add(5 * time.Second); wrong.NumClients() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the wrong store still holds %d clients", wrong.NumClients())
		}
	}
	plain := plainStore(t, pub)
	err = NATSChecker{Port: plain, Dial: 2 * time.Second}.Check(context.Background(), "127.0.0.1", testStorePin, kp)
	if err == nil || errors.Is(err, ErrStoreRefusedKey) {
		t.Fatalf("plaintext store: %v", err)
	}

	right := tlsStore(t, pub, 0, testStore)
	rport := right.Addr().(*net.TCPAddr).Port
	if err := (NATSChecker{Port: rport, Dial: 2 * time.Second}).Check(context.Background(), "127.0.0.1", "AB", kp); err == nil {
		t.Fatal("a malformed pin was accepted")
	}
	seed, _ := kp.Seed()
	if strings.Contains(err.Error(), string(seed)) {
		t.Fatal("the error echoes a seed")
	}
}

// A store restarted with the same disk key keeps its pin, so the worker
// reconnects to it; the serving connection is pinned on reconnect too.
func TestPinnedTLS_SameKeyAfterStoreRestart(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	first := tlsStore(t, pub, 0, testStore)
	port := first.Addr().(*net.TCPAddr).Port
	js, closeConn, err := NATSConnector{Port: port}.Connect(context.Background(), "127.0.0.1", testStorePin, kp)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	first.Shutdown()
	first.WaitForShutdown()

	tlsStore(t, pub, port, testStore)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := js.AccountInfo(ctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reconnect to the restarted store: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := (NATSChecker{Port: port, Dial: 2 * time.Second}).Check(context.Background(), "127.0.0.1", testStorePin, kp); err != nil {
		t.Fatalf("restarted store: %v", err)
	}
}
