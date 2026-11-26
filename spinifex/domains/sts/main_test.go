package sts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// seedStoreDir is a JetStream store holding the IAM and STS buckets and the
// active test accounts. newTestSetup starts each test's server on a copy of it,
// which costs less than creating every bucket again.
var seedStoreDir string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sts-seed-store-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create seed store dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := buildSeedStore(dir); err != nil {
		fmt.Fprintln(os.Stderr, "build seed store:", err)
		return 1
	}
	seedStoreDir = dir
	return m.Run()
}

func newJetStreamServer(storeDir string) (*server.Server, error) {
	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  storeDir,
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		return nil, err
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		return nil, errors.New("embedded NATS server not ready for connections within 5s")
	}
	return ns, nil
}

// buildSeedStore runs the production constructors once against a fresh server,
// so the seeded buckets have the exact config and schema version they create.
func buildSeedStore(dir string) error {
	// One embedded server is a one-node cluster.
	if err := clustersize.Declare(1); err != nil {
		return err
	}
	ns, err := newJetStreamServer(dir)
	if err != nil {
		return err
	}
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		return err
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	masterKey, err := iamdomain.GenerateMasterKey()
	if err != nil {
		return err
	}
	iamSvc, err := iamdomain.NewIAMServiceImpl(ctx, nc, masterKey)
	if err != nil {
		return err
	}
	if _, err := NewSTSServiceImpl(ctx, nc, iamSvc, masterKey, newFakeJWKSResolver()); err != nil {
		return err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	kv, err := js.KeyValue(ctx, iamdomain.KVBucketAccounts)
	if err != nil {
		return err
	}
	for _, id := range []string{testCallerAccountID, testCrossAccountID, "123456789012", "111122223333"} {
		data, err := json.Marshal(iamdomain.Account{AccountID: id, AccountName: "t" + id, Status: iamdomain.AccountStatusActive})
		if err != nil {
			return err
		}
		if _, err := kv.Put(ctx, id, data); err != nil {
			return err
		}
	}
	return nil
}

// startSeededJetStream starts a server of the test's own on a copy of the seed
// store and returns a client connected to it.
func startSeededJetStream(t *testing.T) *nats.Conn {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(seedStoreDir)))
	ns, err := newJetStreamServer(dir)
	require.NoError(t, err)
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}
