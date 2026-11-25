package vm

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// sharedJS is one embedded JetStream server for the package. Each test gets
// its own account, so its buckets and streams are not visible to other tests.
var sharedJS struct {
	mu       sync.Mutex
	srv      *server.Server
	storeDir string
	accounts atomic.Int64
}

// accountPerUser puts a client in the account named by its username.
type accountPerUser struct{ srv *server.Server }

func (a *accountPerUser) Check(c server.ClientAuthentication) bool {
	acc, err := a.srv.LookupAccount(c.GetOpts().Username)
	if err != nil {
		return false
	}
	c.RegisterUser(&server.User{Username: c.GetOpts().Username, Account: acc})
	return true
}

// sharedJetStreamServer returns the shared server, starting it if needed.
func sharedJetStreamServer(t *testing.T) *server.Server {
	t.Helper()
	sharedJS.mu.Lock()
	defer sharedJS.mu.Unlock()
	if sharedJS.srv == nil {
		srv, dir, err := startJetStreamServer()
		require.NoError(t, err)
		sharedJS.srv, sharedJS.storeDir = srv, dir
	}
	return sharedJS.srv
}

// startJetStreamServer starts a JetStream server with no listener. Its store
// outlives any one test, so it is not under t.TempDir.
func startJetStreamServer() (*server.Server, string, error) {
	dir, err := os.MkdirTemp("", "vm-jetstream-")
	if err != nil {
		return nil, "", err
	}
	auth := &accountPerUser{}
	srv, err := server.NewServer(&server.Options{
		JetStream: true, StoreDir: dir, DontListen: true, NoLog: true, NoSigs: true,
		CustomClientAuthentication: auth,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, "", err
	}
	auth.srv = srv
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		_ = os.RemoveAll(dir)
		return nil, "", errors.New("embedded NATS server not ready for connections within 5s")
	}
	return srv, dir, nil
}

// stopSharedJetStream shuts the shared server down and waits for its
// goroutines, so a goroutine leak check sees only what its own test left.
func stopSharedJetStream() {
	sharedJS.mu.Lock()
	defer sharedJS.mu.Unlock()
	if sharedJS.srv != nil {
		sharedJS.srv.Shutdown()
		sharedJS.srv.WaitForShutdown()
		_ = os.RemoveAll(sharedJS.storeDir)
		sharedJS.srv, sharedJS.storeDir = nil, ""
	}
}

// newTestJetStream returns a JetStream handle in a new, empty account on the
// shared server.
func newTestJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	srv := sharedJetStreamServer(t)

	name := fmt.Sprintf("T%d", sharedJS.accounts.Add(1))
	acc, err := srv.RegisterAccount(name)
	require.NoError(t, err)
	require.NoError(t, acc.EnableJetStream(map[string]server.JetStreamAccountLimits{
		"": {MaxMemory: -1, MaxStore: -1, MaxStreams: -1, MaxConsumers: -1},
	}, nil))

	nc, err := nats.Connect("", nats.InProcessServer(srv), nats.UserInfo(name, ""))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	clustersize.DeclareForTest(t, 1)
	return js
}
