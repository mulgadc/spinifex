package elbv2

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
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

// sharedServers are embedded JetStream servers started once for the package.
// Each test connects to one in its own account, so fixed bucket names and
// subjects stay private to it. Several servers spread the server-wide locks.
var sharedServers []*sharedServer

type sharedServer struct {
	srv      *server.Server
	storeDir string

	mu       sync.Mutex
	accounts map[string]*server.Account
}

var _ server.Authentication = (*sharedServer)(nil)

func TestMain(m *testing.M) {
	os.Exit(runWithSharedNATS(m))
}

func runWithSharedNATS(m *testing.M) int {
	// One embedded server is a one-node cluster.
	if err := clustersize.Declare(1); err != nil {
		fmt.Fprintln(os.Stderr, "declare cluster size:", err)
		return 1
	}
	// The embedded servers allocate heavily; fewer collections cut the run's CPU.
	debug.SetGCPercent(400)
	defer func() {
		for _, s := range sharedServers {
			s.stop()
		}
	}()
	for range runtime.GOMAXPROCS(0) {
		s, err := startSharedServer()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		sharedServers = append(sharedServers, s)
	}
	return m.Run()
}

func startSharedServer() (*sharedServer, error) {
	storeDir, err := os.MkdirTemp("", "elbv2-test-nats-")
	if err != nil {
		return nil, fmt.Errorf("nats store dir: %w", err)
	}
	s := &sharedServer{storeDir: storeDir, accounts: map[string]*server.Account{}}
	ns, err := server.NewServer(&server.Options{
		Host:                       "127.0.0.1",
		Port:                       -1,
		JetStream:                  true,
		StoreDir:                   storeDir,
		NoLog:                      true,
		NoSigs:                     true,
		CustomClientAuthentication: s,
	})
	if err != nil {
		_ = os.RemoveAll(storeDir)
		return nil, fmt.Errorf("new nats server: %w", err)
	}
	s.srv = ns
	ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		s.stop()
		return nil, fmt.Errorf("embedded NATS server not ready for connections within 10s")
	}
	return s, nil
}

func (s *sharedServer) stop() {
	s.srv.Shutdown()
	s.srv.WaitForShutdown()
	_ = os.RemoveAll(s.storeDir)
}

// Check gives every new username its own JetStream-enabled account.
func (s *sharedServer) Check(c server.ClientAuthentication) bool {
	name := c.GetOpts().Username
	if name == "" {
		return false
	}
	acc, err := s.accountFor(name)
	if err != nil {
		return false
	}
	c.RegisterUser(&server.User{Username: name, Account: acc})
	return true
}

func (s *sharedServer) accountFor(name string) (*server.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc, ok := s.accounts[name]; ok {
		return acc, nil
	}
	acc, err := s.srv.RegisterAccount(name)
	if err != nil {
		return nil, err
	}
	if err := acc.EnableJetStream(nil, nil); err != nil {
		return nil, err
	}
	s.accounts[name] = acc
	return acc, nil
}

// release stops the account's streams and deletes its store, so a finished
// test leaves nothing running or on disk.
func (s *sharedServer) release(name string) error {
	s.mu.Lock()
	acc, ok := s.accounts[name]
	delete(s.accounts, name)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if err := acc.DisableJetStream(); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.storeDir, server.JetStreamStoreDir, name))
}

var accountSeq atomic.Uint64

// startTestJetStream returns a connection to a shared server in a fresh,
// empty account, closed and released when t ends.
func startTestJetStream(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	require.NotEmpty(t, sharedServers, "shared NATS servers are not running")
	seq := accountSeq.Add(1)
	s := sharedServers[seq%uint64(len(sharedServers))]
	name := fmt.Sprintf("acct%d", seq)
	nc, err := nats.Connect("", nats.InProcessServer(s.srv), nats.UserInfo(name, "x"))
	require.NoError(t, err)
	t.Cleanup(func() {
		nc.Close()
		require.NoError(t, s.release(name))
	})
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return nc, js
}
