//go:build integration

package integration

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// sharedNATSHarness is the package-wide handle TestMain installs before
// m.Run() and every StartGateway call reads from.
var sharedNATSHarness *sharedNATS

// sharedNATS owns the one embedded NATS+JetStream server shared by every
// test in the package, plus the dynamic per-account authenticator that
// gives each test its own isolated JetStream namespace on that server. See
// the TestMain doc comment (main_test.go) for why the server — and every
// connection into it — is shared across the whole package rather than
// booted fresh per test.
type sharedNATS struct {
	srv  *server.Server
	auth *accountAuthenticator

	connsMu sync.Mutex
	conns   []*nats.Conn
}

// startSharedNATS boots the one embedded, JetStream-enabled NATS server the
// whole package's tests connect into.
func startSharedNATS() (*sharedNATS, error) {
	storeDir, err := os.MkdirTemp("", "spinifex-integration-nats-")
	if err != nil {
		return nil, fmt.Errorf("nats store tempdir: %w", err)
	}

	auth := newAccountAuthenticator()
	opts := &server.Options{
		Host:                       "127.0.0.1",
		Port:                       -1,
		JetStream:                  true,
		StoreDir:                   storeDir,
		NoLog:                      true,
		NoSigs:                     true,
		CustomClientAuthentication: auth,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("new nats server: %w", err)
	}
	auth.srv = ns

	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		return nil, fmt.Errorf("nats server not ready for connections")
	}

	return &sharedNATS{srv: ns, auth: auth}, nil
}

// connectIsolated opens a fresh connection scoped to its own dynamically
// provisioned NATS account keyed by name, giving the caller a JetStream
// namespace no other test's account can see or collide with. The returned
// connection is tracked and closed by TestMain once every test has
// finished, not by the caller — see the TestMain doc comment for why.
func (h *sharedNATS) connectIsolated(name string) (*nats.Conn, nats.JetStreamContext, error) {
	nc, err := nats.Connect(h.srv.ClientURL(), nats.UserInfo(name, "x"))
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream: %w", err)
	}

	h.connsMu.Lock()
	h.conns = append(h.conns, nc)
	h.connsMu.Unlock()
	return nc, js, nil
}

// closeAll closes every connection opened via connectIsolated. Only called
// from TestMain after every test has finished.
func (h *sharedNATS) closeAll() {
	h.connsMu.Lock()
	defer h.connsMu.Unlock()
	for _, nc := range h.conns {
		nc.Close()
	}
}

// accountAuthenticator implements nats-server's Authentication interface
// (wired in via Options.CustomClientAuthentication). Every distinct
// username that connects gets its own dedicated, JetStream-enabled NATS
// account, created on first sight and reused after. This is what gives each
// test a private JetStream namespace on the one shared server: fixed KV
// bucket names production services hard-code (e.g. IAM's "iam-users")
// resolve to a distinct JetStream store per account instead of one global
// bucket every test would otherwise collide on. NATS subjects are equally
// account-scoped by default, so per-test daemon-side stubs (StubSubject)
// stay isolated the same way they were when each test had its own server.
type accountAuthenticator struct {
	srv *server.Server

	mu       sync.Mutex
	accounts map[string]*server.Account
}

func newAccountAuthenticator() *accountAuthenticator {
	return &accountAuthenticator{accounts: make(map[string]*server.Account)}
}

// Check implements server.Authentication. The account name is the username
// the client connected with (see accountNameFor), so it is only ever
// created once even if a test somehow reconnected.
func (a *accountAuthenticator) Check(c server.ClientAuthentication) bool {
	name := c.GetOpts().Username
	if name == "" {
		return false
	}
	acc, err := a.accountFor(name)
	if err != nil {
		return false
	}
	c.RegisterUser(&server.User{Username: name, Account: acc})
	return true
}

func (a *accountAuthenticator) accountFor(name string) (*server.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if acc, ok := a.accounts[name]; ok {
		return acc, nil
	}
	acc, err := a.srv.RegisterAccount(name)
	if err != nil {
		return nil, fmt.Errorf("register account %s: %w", name, err)
	}
	if err := acc.EnableJetStream(nil, nil); err != nil {
		return nil, fmt.Errorf("enable jetstream for account %s: %w", name, err)
	}
	a.accounts[name] = acc
	return acc, nil
}

// accountNameReplacer strips characters NATS account names / on-disk
// JetStream store directories don't tolerate well (subtests separate their
// path with "/", and some test names contain spaces).
var accountNameReplacer = strings.NewReplacer("/", "_", " ", "_")

// accountSeq disambiguates repeat runs of one test. t.Name() is unique among
// tests in a binary but NOT across passes: -count=N replays identical names
// inside the same TestMain, so a name-only account key hands the second pass
// the first pass's JetStream namespace. That namespace still holds an ECR
// signing key sealed with the first pass's master key, which StartGateway
// regenerates per call — so the reused key failed to decrypt and took down
// every gateway-starting test on the second pass.
var accountSeq atomic.Uint64

// accountNameFor derives a fresh NATS account name for t, so each gateway gets
// an empty JetStream namespace no matter how often the test is replayed.
func accountNameFor(t *testing.T) string {
	return fmt.Sprintf("%s_%d", accountNameReplacer.Replace(t.Name()), accountSeq.Add(1))
}
